// Package auth provides authentication and authorisation middleware for maniflex.
//
// None of these middlewares are registered automatically — import and register
// whichever ones you need:
//
//	import "github.com/xaleel/maniflex/middleware/auth"
//
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret))
//	server.Pipeline.Auth.Register(auth.RequireRole("admin"), maniflex.ForModel("User"))
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
)

// ── JWTAuth ───────────────────────────────────────────────────────────────────

// JWTOptions configures JWTAuth behaviour.
type JWTOptions struct {
	// Header is the HTTP header to read the token from.
	// Default: "Authorization" (value must be "Bearer <token>").
	Header string

	// ClockSkew allows tokens to be this far past their expiry.
	// Default: 0 (strict).
	ClockSkew time.Duration

	// UserIDClaim is the JWT claim used to populate ctx.Auth.UserID.
	// Default: "sub".
	UserIDClaim string

	// RolesClaim is the JWT claim used to populate ctx.Auth.Roles.
	// The claim value may be a []string or a space-separated string.
	// Default: "roles".
	RolesClaim string

	// TenantClaim is the JWT claim copied into ctx.Auth.TenantID.
	// Default: "" (disabled — TenantID is left empty).
	TenantClaim string

	// ScopesClaim is the JWT claim used to populate ctx.Auth.Scopes.
	// The claim value may be a []string or a space-separated string (RFC 8693).
	// Default: "scope".
	ScopesClaim string

	// Optional: if set, tokens are only accepted when their "iss" equals this.
	Issuer string

	// Optional: if set, tokens are only accepted when their "aud" contains this.
	Audience string

	// PublicKey enables asymmetric signature verification. When non-nil the
	// secret argument is ignored and the JWT header "alg" must match the key:
	//
	//   *rsa.PublicKey   → RS256 / RS384 / RS512 (PKCS#1 v1.5)
	//   *ecdsa.PublicKey → ES256 (P-256) / ES384 (P-384) / ES512 (P-521)
	//
	// When nil, HMAC-SHA256 (HS256) is used with the secret.
	PublicKey crypto.PublicKey
}

func (o *JWTOptions) applyDefaults() {
	if o.Header == "" {
		o.Header = "Authorization"
	}
	if o.UserIDClaim == "" {
		o.UserIDClaim = "sub"
	}
	if o.RolesClaim == "" {
		o.RolesClaim = "roles"
	}
	if o.ScopesClaim == "" {
		o.ScopesClaim = "scope"
	}
}

// JWTAuth validates a signed JWT and populates ctx.Auth.
//
// By default it verifies HMAC-SHA256 (HS256) using the supplied secret. To
// verify asymmetric tokens issued by an identity provider (Keycloak, Auth0,
// Cognito, etc.) set JWTOptions.PublicKey to an *rsa.PublicKey for RS256/384/512
// or an *ecdsa.PublicKey for ES256/384/512; the secret argument is then ignored.
//
//	server.Pipeline.Auth.Register(auth.JWTAuth("mysecret"))
//	server.Pipeline.Auth.Register(auth.JWTAuth("mysecret", auth.JWTOptions{
//	    Issuer:   "https://myapp.com",
//	    Audience: "api",
//	}))
//	server.Pipeline.Auth.Register(auth.JWTAuth("", auth.JWTOptions{
//	    PublicKey: rsaPub, // RS256
//	}))
func JWTAuth(secret string, opts ...JWTOptions) maniflex.MiddlewareFunc {
	opt := JWTOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.applyDefaults()
	return jwtMiddleware(secret, opt, staticResolver(opt.PublicKey))
}

// keyResolver returns the verification key for a token given its header "kid"
// and "alg". A nil key means "verify with the HMAC secret". The static path
// ignores kid; the JWKS path selects by kid.
type keyResolver func(kid, alg string) (crypto.PublicKey, error)

// staticResolver always returns the configured key (nil for HMAC), ignoring kid.
func staticResolver(pub crypto.PublicKey) keyResolver {
	return func(string, string) (crypto.PublicKey, error) { return pub, nil }
}

// jwtMiddleware is the shared verification core used by both JWTAuth (static
// key) and JWKSAuth (kid-selected key from a rotating JWK Set).
func jwtMiddleware(secret string, opt JWTOptions, resolve keyResolver) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		raw := ctx.Request.Header.Get(opt.Header)
		if opt.Header == "Authorization" {
			if !strings.HasPrefix(raw, "Bearer ") {
				ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
					"missing or malformed Authorization: Bearer <token> header")
				return nil
			}
			raw = strings.TrimPrefix(raw, "Bearer ")
		}
		if raw == "" {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
			return nil
		}

		claims, err := parseJWT(raw, secret, resolve)
		if err != nil {
			ctx.Abort(http.StatusUnauthorized, "INVALID_TOKEN", err.Error())
			return nil
		}

		now := time.Now()

		// Expiry
		if exp, ok := claims["exp"]; ok {
			expF, _ := toFloat64(exp)
			if time.Unix(int64(expF), 0).Add(opt.ClockSkew).Before(now) {
				ctx.Abort(http.StatusUnauthorized, "TOKEN_EXPIRED", "token has expired")
				return nil
			}
		}

		// Not-before (RFC 7519 §4.1.5). A token bearing nbf in the future
		// is not yet valid. Apply the same ClockSkew tolerance as exp.
		if nbf, ok := claims["nbf"]; ok {
			nbfF, _ := toFloat64(nbf)
			if time.Unix(int64(nbfF), 0).Add(-opt.ClockSkew).After(now) {
				ctx.Abort(http.StatusUnauthorized, "TOKEN_NOT_YET_VALID", "token not yet valid")
				return nil
			}
		}

		// Issued-at (RFC 7519 §4.1.6). An iat in the future is clock-skew
		// or a bad issuer; reject so the request can't ride on a token that
		// claims to have been issued in the future.
		if iat, ok := claims["iat"]; ok {
			iatF, _ := toFloat64(iat)
			if time.Unix(int64(iatF), 0).Add(-opt.ClockSkew).After(now) {
				ctx.Abort(http.StatusUnauthorized, "TOKEN_FUTURE_ISSUED", "token issued in the future")
				return nil
			}
		}

		// Issuer
		if opt.Issuer != "" {
			if iss, _ := claims["iss"].(string); iss != opt.Issuer {
				ctx.Abort(http.StatusUnauthorized, "INVALID_TOKEN", "invalid issuer")
				return nil
			}
		}

		// Audience
		if opt.Audience != "" {
			if !audienceContains(claims["aud"], opt.Audience) {
				ctx.Abort(http.StatusUnauthorized, "INVALID_TOKEN", "invalid audience")
				return nil
			}
		}

		userID, _ := claims[opt.UserIDClaim].(string)
		roles := extractRoles(claims[opt.RolesClaim])
		scopes := extractRoles(claims[opt.ScopesClaim]) // same extraction logic
		sessionID, _ := claims["jti"].(string)

		var tenantID string
		if opt.TenantClaim != "" {
			tenantID, _ = claims[opt.TenantClaim].(string)
		}

		ctx.Auth = &maniflex.AuthInfo{
			UserID:     userID,
			Roles:      roles,
			Claims:     claims,
			TenantID:   tenantID,
			Scopes:     scopes,
			SessionID:  sessionID,
			AuthMethod: "jwt",
		}
		return next()
	}
}

// ── APIKeyAuth ─────────────────────────────────────────────────────────────────

// APIKeyEntry maps an API key string to the AuthInfo it grants.
type APIKeyEntry struct {
	Key  string
	Auth maniflex.AuthInfo
}

// APIKeyAuth validates a static API key from a request header.
// It accepts a variadic list of APIKeyEntry values so different keys can carry
// different identities and roles.
//
//	server.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
//	    auth.APIKeyEntry{Key: "abc123", Auth: maniflex.AuthInfo{UserID: "svc-1", Roles: []string{"admin"}}},
//	    auth.APIKeyEntry{Key: "xyz789", Auth: maniflex.AuthInfo{UserID: "svc-2", Roles: []string{"reader"}}},
//	))
func APIKeyAuth(header string, entries ...APIKeyEntry) maniflex.MiddlewareFunc {
	index := make(map[string]maniflex.AuthInfo, len(entries))
	for _, e := range entries {
		index[e.Key] = e.Auth
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		key := ctx.Request.Header.Get(header)
		if key == "" {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
				fmt.Sprintf("missing %s header", header))
			return nil
		}
		info, ok := index[key]
		if !ok {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "invalid API key")
			return nil
		}
		if info.AuthMethod == "" {
			info.AuthMethod = "api_key"
		}
		ctx.Auth = &info
		return next()
	}
}

// ── RequireRole ────────────────────────────────────────────────────────────────

// RequireRole returns 403 unless ctx.Auth holds at least one of the given roles.
// Must be registered After an auth middleware that populates ctx.Auth.
//
//	server.Pipeline.Auth.Register(auth.RequireRole("admin", "editor"),
//	    maniflex.ForModel("Post"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	)
func RequireRole(roles ...string) maniflex.MiddlewareFunc {
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth == nil {
			ctx.Abort(http.StatusForbidden, "FORBIDDEN", "authentication required")
			return nil
		}
		for _, r := range ctx.Auth.Roles {
			if roleSet[r] {
				return next()
			}
		}
		ctx.Abort(http.StatusForbidden, "FORBIDDEN",
			fmt.Sprintf("one of the following roles is required: %s", strings.Join(roles, ", ")))
		return nil
	}
}

// ── RequireOwner ───────────────────────────────────────────────────────────────

// RequireOwner enforces that the authenticated user owns the resource being
// written or read. On create it sets ownerField = ctx.Auth.UserID automatically.
// On update/delete it fetches the record and compares ownerField to ctx.Auth.UserID.
//
// Users with a role in adminRoles bypass ownership checks entirely.
//
//	server.Pipeline.Auth.Register(auth.RequireOwner("user_id", "admin"))
func RequireOwner(ownerField string, adminRoles ...string) maniflex.MiddlewareFunc {
	adminSet := make(map[string]bool, len(adminRoles))
	for _, r := range adminRoles {
		adminSet[r] = true
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth == nil {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return nil
		}
		// Admins bypass ownership
		for _, r := range ctx.Auth.Roles {
			if adminSet[r] {
				return next()
			}
		}

		switch ctx.Operation {
		case maniflex.OpCreate:
			// Inject the owner field. SetField writes through to both ParsedBody
			// and the typed record (ctx.Record), so later typed middleware and the
			// struct write path see the injected owner.
			ctx.SetField(ownerField, ctx.Auth.UserID)

		case maniflex.OpUpdate, maniflex.OpDelete, maniflex.OpRead:
			// The DB step hasn't run yet, so we must compare against what was
			// stored in ctx.DBResult if this is an After-DB middleware.
			// As a Before-DB middleware we can only set a forced filter.
			// We encode ownership as a ForceFilter so the DB step handles it.
			ctx.Set("_require_owner_field", ownerField)
			ctx.Set("_require_owner_value", ctx.Auth.UserID)
		}
		return next()
	}
}

// ── AllowPublicRead ────────────────────────────────────────────────────────────

// AllowPublicRead is a passthrough on OpRead and OpList; on all other operations
// it requires ctx.Auth to be populated (i.e. another auth middleware must run first
// for writes). Register this After any auth middleware.
//
//	server.Pipeline.Auth.Register(auth.JWTAuth(secret))
//	server.Pipeline.Auth.Register(auth.AllowPublicRead())
func AllowPublicRead() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Operation == maniflex.OpRead || ctx.Operation == maniflex.OpList {
			return next()
		}
		if ctx.Auth == nil {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
				"authentication required for write operations")
			return nil
		}
		return next()
	}
}

// ── BlockOperation ─────────────────────────────────────────────────────────────

// BlockOperation returns 405 Method Not Allowed for the named operations.
// Use it to make a model effectively read-only, or to disable specific verbs.
//
//	// Read-only model
//	server.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.ForModel("AuditLog"),
//	)
func BlockOperation(ops ...maniflex.Operation) maniflex.MiddlewareFunc {
	blocked := make(map[maniflex.Operation]bool, len(ops))
	for _, op := range ops {
		blocked[op] = true
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if blocked[ctx.Operation] {
			ctx.Abort(http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED",
				fmt.Sprintf("operation %q is not allowed on %s", ctx.Operation, ctx.Model.Name))
			return nil
		}
		return next()
	}
}

// ── JWT internals ─────────────────────────────────────────────────────────────

// parseJWT validates a JWT signature against either a shared HMAC secret or an
// asymmetric public key (RSA / ECDSA) and returns the decoded claims. The
// signing algorithm is selected from the token header and must match the key
// material supplied; mismatches are rejected to avoid algorithm-confusion
// attacks (e.g. an HS256 token forged against an RSA public key).
func parseJWT(token, secret string, resolve keyResolver) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed token: expected 3 parts")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed token header")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("malformed token header: %w", err)
	}

	signingInput := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed token signature")
	}

	pub, err := resolve(hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(hdr.Alg, signingInput, sig, secret, pub); err != nil {
		return nil, err
	}

	claimBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed token claims")
	}
	var claims map[string]any
	if err := json.Unmarshal(claimBytes, &claims); err != nil {
		return nil, fmt.Errorf("malformed token claims: %w", err)
	}
	return claims, nil
}

func verifySignature(alg string, signingInput, sig []byte, secret string, pub crypto.PublicKey) error {
	switch pub.(type) {
	case nil:
		if alg != "HS256" {
			return fmt.Errorf("unexpected algorithm %q (HS256 required)", alg)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(signingInput)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return fmt.Errorf("invalid token signature")
		}
		return nil
	}

	hashFn, cryptoHash, err := hashForAlg(alg)
	if err != nil {
		return err
	}
	hashFn.Write(signingInput)
	digest := hashFn.Sum(nil)

	switch k := pub.(type) {
	case *rsa.PublicKey:
		if !strings.HasPrefix(alg, "RS") {
			return fmt.Errorf("unexpected algorithm %q for RSA key", alg)
		}
		if err := rsa.VerifyPKCS1v15(k, cryptoHash, digest, sig); err != nil {
			return fmt.Errorf("invalid token signature")
		}
		return nil
	case *ecdsa.PublicKey:
		if !strings.HasPrefix(alg, "ES") {
			return fmt.Errorf("unexpected algorithm %q for ECDSA key", alg)
		}
		// JWS ECDSA signatures are r||s, fixed-width per curve.
		keySize := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*keySize {
			return fmt.Errorf("invalid ECDSA signature length")
		}
		r := new(big.Int).SetBytes(sig[:keySize])
		s := new(big.Int).SetBytes(sig[keySize:])
		if !ecdsa.Verify(k, digest, r, s) {
			return fmt.Errorf("invalid token signature")
		}
		return nil
	default:
		return fmt.Errorf("unsupported PublicKey type %T", pub)
	}
}

func hashForAlg(alg string) (hash.Hash, crypto.Hash, error) {
	switch alg {
	case "RS256", "ES256":
		return sha256.New(), crypto.SHA256, nil
	case "RS384", "ES384":
		return sha512.New384(), crypto.SHA384, nil
	case "RS512", "ES512":
		return sha512.New(), crypto.SHA512, nil
	default:
		return nil, 0, fmt.Errorf("unsupported algorithm %q", alg)
	}
}

func extractRoles(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, r := range t {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	}
	return nil
}

func audienceContains(aud any, target string) bool {
	switch a := aud.(type) {
	case string:
		return a == target
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok && s == target {
				return true
			}
		}
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
