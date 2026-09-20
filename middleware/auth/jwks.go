package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xaleel/maniflex"
)

const (
	// defaultJWKSCacheTTL is how long a fetched JWK Set is considered fresh.
	defaultJWKSCacheTTL = time.Hour
	// defaultJWKSMinRefetch rate-limits refetches so an unknown-kid storm can't
	// hammer the issuer's JWKS endpoint. It is also the ceiling on the failure
	// backoff below.
	defaultJWKSMinRefetch = 5 * time.Minute
	// defaultJWKSMinRetry is how long the cache waits after a *failed* fetch
	// before trying again, doubling up to defaultJWKSMinRefetch. It starts small
	// because an empty cache means no token verifies at all: a blip while the
	// process is starting should cost a second, not the full refetch interval.
	defaultJWKSMinRetry = time.Second
	// maxJWKSBody caps the JWK Set response. An RSA-2048 JWK is roughly 400
	// bytes, so this is thousands of signing keys — far past any real issuer,
	// and the point is only that a hostile or broken endpoint cannot make the
	// process read an unbounded body into memory.
	maxJWKSBody = 1 << 20
)

// JWKSAuth validates asymmetric JWTs against a rotating JWK Set published at
// jwksURL (e.g. an identity provider's /.well-known/jwks.json). Signing keys are
// fetched, cached, and selected by the token header's "kid"; an unknown kid
// triggers a rate-limited refetch so a rotated key is picked up without a
// redeploy. RSA (RS256/384/512) and EC (ES256/384/512) keys are supported.
//
// The kid is read before the signature is checked, so unauthenticated callers
// influence when a fetch happens: concurrent refreshes are collapsed into one
// request, and failures back off, so a token storm costs the issuer little.
//
// All other JWTOptions (Issuer, Audience, claim mappings, ClockSkew) apply as
// with JWTAuth. The static-key JWTAuth remains for a fixed key or offline tests.
//
//	server.Pipeline.Auth.Register(auth.JWKSAuth(
//	    "https://issuer.example.com/.well-known/jwks.json",
//	    auth.JWTOptions{Issuer: "https://issuer.example.com", Audience: "api"}))
func JWKSAuth(jwksURL string, opts ...JWTOptions) maniflex.MiddlewareFunc {
	if reason := plaintextJWKSWarning(jwksURL); reason != "" {
		// At construction, because nothing fetches the JWK Set until the first
		// token is validated — so a plaintext URL is otherwise invisible until
		// production traffic arrives, and then it does not fail, it succeeds.
		slog.Default().Warn("auth.JWKSAuth: "+reason,
			slog.String("url", jwksURL),
			slog.String("why", "the JWK Set is the only thing deciding which tokens verify, so "+
				"anyone able to intercept the fetch can substitute their own key and mint tokens "+
				"with any claims they like"),
			slog.String("hint", "use https; an issuer on loopback is exempt for local development"))
	}
	opt := JWTOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.applyDefaults()
	if opt.Audience == "" {
		// An identity provider signs for every application in its tenant with
		// the same keys, so "the signature verifies against the IdP's JWKS" does
		// not mean "this token was minted for us". Without an audience check the
		// server accepts a token issued to any other client of that IdP —
		// audience confusion — and ValidateProduction cannot see the option to
		// say so, since a middleware is an opaque closure to it (audit AUTH-8).
		slog.Default().Warn("auth.JWKSAuth: no Audience configured",
			slog.String("url", jwksURL),
			slog.String("why", "the issuer signs tokens for its other clients with these same keys, "+
				"so without an audience check a token minted for one of them verifies here too"),
			slog.String("hint", "set JWTOptions.Audience to this API's identifier at the issuer"))
	}
	cache := newJWKSCache(jwksURL)
	// secret is unused for JWKS (keys are asymmetric); the resolver supplies the key.
	return jwtMiddleware("", opt, cache.key)
}

// jwksCache fetches and caches a JWK Set, resolving public keys by kid.
type jwksCache struct {
	url        string
	client     *http.Client
	ttl        time.Duration
	minRefetch time.Duration
	minRetry   time.Duration

	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	// lastErr is why the most recent fetch failed, nil when it succeeded. It is
	// what a throttled caller is told, so that backing off reads as "the issuer
	// is unreachable" rather than "no signing key for that kid".
	lastErr error
	// backoff is the current wait after a failure, 0 while healthy.
	backoff time.Duration
	// inFlight is closed when the fetch running right now finishes; nil when
	// none is. It is the single-flight latch: key() is reached before a token's
	// signature is checked, so an unauthenticated caller decides how often this
	// runs, and without the latch every one of them opened its own connection
	// to the issuer.
	inFlight chan struct{}
}

func newJWKSCache(url string) *jwksCache {
	return &jwksCache{
		url: url,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: jwksCheckRedirect,
		},
		ttl:        defaultJWKSCacheTTL,
		minRefetch: defaultJWKSMinRefetch,
		minRetry:   defaultJWKSMinRetry,
		keys:       map[string]crypto.PublicKey{},
	}
}

// plaintextJWKSWarning returns the reason the JWK Set at rawURL would be
// fetched over an untrustworthy transport, or "" when it is sound.
//
// The JWK Set is the entire root of trust for JWKSAuth: unlike JWTAuth there is
// no shared secret, and the authentication decision reduces to whether the
// token's signature verifies against a key from this URL. Issuer and audience
// are claims inside the token, checked only after that signature verifies, so
// they are the attacker's to choose too. Fetched over plaintext, an active
// network attacker returns a JWK Set holding their own public key and mints
// tokens for any identity they like — not privilege escalation from a stolen
// account but arbitrary identities, including ones that never existed (S11).
//
// Two things here make a transient intercept a lasting one, which is why this
// is worth a warning rather than a docs note: refresh replaces the whole key
// map and caches it for defaultJWKSCacheTTL, and key() falls back to a cached
// key of any age when a later refresh fails.
//
// Loopback is exempt rather than warned about. A local Keycloak or dex on
// http://localhost is an ordinary development setup with no wire to sit on, and
// a warning that fires on every dev run is one nobody reads by the time it
// matters. A private LAN address is not exempt: 192.168.0.0/16 is still another
// host, reached over a network somebody may be on.
func plaintextJWKSWarning(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "JWKS URL is not a valid absolute URL"
	}
	switch u.Scheme {
	case "https":
		return ""
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return ""
		}
		return "JWK Set is fetched over plaintext HTTP from a non-loopback host"
	default:
		return fmt.Sprintf("JWKS URL scheme %q is neither http nor https", u.Scheme)
	}
}

// isLoopbackHost reports whether host names this machine. url.URL.Hostname has
// already stripped the port and the brackets around a literal IPv6 address.
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// jwksCheckRedirect is the fetching client's redirect policy.
//
// A scheme check on the configured URL does not survive a redirect on its own:
// Go's default client follows https -> http without complaint, stripping
// Authorization across hosts but permitting the downgrade itself. So an issuer
// that redirects can move the key material that decides authentication onto
// plaintext while the configured https:// URL reads as though nothing happened.
func jwksCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("fetch JWKS: stopped after 10 redirects")
	}
	// Compare against the hop just taken rather than the original request, so a
	// downgrade anywhere along a chain is caught rather than only at its head.
	return checkJWKSRedirect(via[len(via)-1].URL, req.URL)
}

// checkJWKSRedirect refuses a redirect that would fetch the JWK Set over a
// weaker transport than the previous hop used.
//
// Landing on plaintext loopback is refused too when the hop before it was
// https. Loopback is exempt as a configured destination, where it means "the
// IdP is on this machine"; arriving there by redirect from a TLS endpoint means
// something else entirely and is never what the operator asked for.
func checkJWKSRedirect(from, to *url.URL) error {
	if to.Scheme == "https" {
		return nil
	}
	if from.Scheme == "https" {
		return fmt.Errorf("fetch JWKS: refusing redirect from %s to %s: "+
			"the JWK Set decides which tokens verify and must not be fetched over plaintext",
			from.Scheme+"://"+from.Host, to.Scheme+"://"+to.Host)
	}
	if !isLoopbackHost(to.Hostname()) {
		return fmt.Errorf("fetch JWKS: refusing redirect to plaintext non-loopback host %s: "+
			"the JWK Set decides which tokens verify", to.Host)
	}
	return nil
}

// key resolves the public key for kid, (re)fetching the JWK Set when the cache
// is empty/stale or when kid is unknown (a likely rotation). It satisfies the
// keyResolver signature; alg is unused (the key type drives verification).
func (c *jwksCache) key(kid, _ string) (crypto.PublicKey, error) {
	c.mu.RLock()
	k, ok := c.keys[kid]
	fresh := !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl
	c.mu.RUnlock()
	if ok && fresh {
		return k, nil
	}

	if err := c.refresh(); err != nil {
		// Refresh failed — fall back to a cached key if we still have one.
		c.mu.RLock()
		k, ok = c.keys[kid]
		c.mu.RUnlock()
		if ok {
			return k, nil
		}
		return nil, err
	}

	c.mu.RLock()
	k, ok = c.keys[kid]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no signing key for kid %q in JWKS", kid)
	}
	return k, nil
}

// throttled reports whether another fetch right now would be a stampede rather
// than a refresh. Callers hold c.mu.
//
// The two cases are genuinely different and the previous single condition could
// only express one of them. After a *success*, the question is how often an
// unknown kid may trigger a refetch while the cached set is still usable, and
// the answer is minRefetch. After a *failure*, the question is how fast to
// retry an issuer that is down — and there the old condition, which also
// required a non-empty fresh cache, was never satisfied, so nothing was
// throttled at all and every request re-fetched.
func (c *jwksCache) throttled() bool {
	if c.lastAttempt.IsZero() {
		return false
	}
	since := time.Since(c.lastAttempt)
	if c.lastErr != nil {
		return since < c.backoff
	}
	return since < c.minRefetch && !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl
}

// refresh fetches the JWK Set, collapsing concurrent callers onto one request
// and backing off exponentially while the issuer is failing.
//
// It returns the error of the fetch it ran, waited on, or is backing off from,
// so a throttled caller learns the issuer is unreachable instead of being told
// the kid is unknown. key() still prefers a cached key of any age over that
// error, which is what keeps a live IdP outage from logging everybody out.
func (c *jwksCache) refresh() error {
	c.mu.Lock()
	if c.throttled() {
		err := c.lastErr
		c.mu.Unlock()
		return err
	}
	if wait := c.inFlight; wait != nil {
		// Someone is already fetching. Wait for their result rather than opening
		// a second connection to the issuer for the same JWK Set.
		c.mu.Unlock()
		<-wait
		c.mu.RLock()
		err := c.lastErr
		c.mu.RUnlock()
		return err
	}
	done := make(chan struct{})
	c.inFlight = done
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	// Seeded with a failure so that a panic in the fetch still releases the
	// latch, records a backoff and leaves the cached keys alone. Leaving
	// inFlight set would park every later caller on a channel nobody closes.
	var keys map[string]crypto.PublicKey
	err := errors.New("fetch JWKS: aborted")
	defer func() {
		c.mu.Lock()
		c.lastErr = err
		switch {
		case err == nil:
			c.keys = keys
			c.fetchedAt = time.Now()
			c.backoff = 0
		case c.backoff == 0:
			c.backoff = c.minRetry
		default:
			if c.backoff *= 2; c.backoff > c.minRefetch {
				c.backoff = c.minRefetch
			}
		}
		c.inFlight = nil
		c.mu.Unlock()
		close(done)
	}()

	keys, err = c.fetch()
	return err
}

// fetch performs one JWK Set request. It does no locking and no throttling —
// refresh owns both — and it does not touch the cache.
func (c *jwksCache) fetch() (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: unexpected status %d", resp.StatusCode)
	}
	// One byte past the cap, so an oversized set is reported as oversized rather
	// than as a JSON syntax error from a body cut off mid-object.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBody+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	if len(body) > maxJWKSBody {
		return nil, fmt.Errorf("fetch JWKS: response larger than %d bytes", maxJWKSBody)
	}
	return parseJWKS(body)
}

// jwk is a single JSON Web Key (RFC 7517) — the subset we verify with.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"` // RSA modulus
	E   string `json:"e"` // RSA exponent
	Crv string `json:"crv"`
	X   string `json:"x"` // EC x
	Y   string `json:"y"` // EC y
}

// parseJWKS decodes a JWK Set into a kid → public key map. Keys marked for
// encryption (use != "sig") and keys of unsupported types are skipped.
func parseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	out := make(map[string]crypto.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil || pub == nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("JWKS contained no usable signing keys")
	}
	return out, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nBytes, err := b64urlDecode(k.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := b64urlDecode(k.E)
		if err != nil {
			return nil, err
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e == 0 {
			return nil, fmt.Errorf("invalid RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
	case "EC":
		crv, err := curveForName(k.Crv)
		if err != nil {
			return nil, err
		}
		xB, err := b64urlDecode(k.X)
		if err != nil {
			return nil, err
		}
		yB, err := b64urlDecode(k.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: crv, X: new(big.Int).SetBytes(xB), Y: new(big.Int).SetBytes(yB)}, nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

func curveForName(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	}
	return nil, fmt.Errorf("unsupported EC curve %q", crv)
}

// b64urlDecode accepts both padded and unpadded base64url (issuers vary).
func b64urlDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
