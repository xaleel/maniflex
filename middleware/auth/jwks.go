package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/xaleel/maniflex"
)

const (
	// defaultJWKSCacheTTL is how long a fetched JWK Set is considered fresh.
	defaultJWKSCacheTTL = time.Hour
	// defaultJWKSMinRefetch rate-limits refetches so an unknown-kid storm can't
	// hammer the issuer's JWKS endpoint.
	defaultJWKSMinRefetch = 5 * time.Minute
)

// JWKSAuth validates asymmetric JWTs against a rotating JWK Set published at
// jwksURL (e.g. an identity provider's /.well-known/jwks.json). Signing keys are
// fetched, cached, and selected by the token header's "kid"; an unknown kid
// triggers a rate-limited refetch so a rotated key is picked up without a
// redeploy. RSA (RS256/384/512) and EC (ES256/384/512) keys are supported.
//
// All other JWTOptions (Issuer, Audience, claim mappings, ClockSkew) apply as
// with JWTAuth. The static-key JWTAuth remains for a fixed key or offline tests.
//
//	server.Pipeline.Auth.Register(auth.JWKSAuth(
//	    "https://issuer.example.com/.well-known/jwks.json",
//	    auth.JWTOptions{Issuer: "https://issuer.example.com", Audience: "api"}))
func JWKSAuth(jwksURL string, opts ...JWTOptions) maniflex.MiddlewareFunc {
	opt := JWTOptions{}
	if len(opts) > 0 {
		opt = opts[0]
	}
	opt.applyDefaults()
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

	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

func newJWKSCache(url string) *jwksCache {
	return &jwksCache{
		url:        url,
		client:     &http.Client{Timeout: 10 * time.Second},
		ttl:        defaultJWKSCacheTTL,
		minRefetch: defaultJWKSMinRefetch,
		keys:       map[string]crypto.PublicKey{},
	}
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

// refresh fetches the JWK Set, rate-limited by minRefetch while the cache is
// still fresh so an unknown-kid storm doesn't stampede the issuer.
func (c *jwksCache) refresh() error {
	c.mu.Lock()
	if !c.lastAttempt.IsZero() &&
		time.Since(c.lastAttempt) < c.minRefetch &&
		!c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < c.ttl {
		c.mu.Unlock()
		return nil
	}
	c.lastAttempt = time.Now()
	c.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read JWKS: %w", err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = time.Now()
	c.mu.Unlock()
	return nil
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
