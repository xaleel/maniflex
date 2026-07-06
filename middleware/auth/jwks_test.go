package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signRS256 mints a compact JWT signed RS256, with an optional kid header.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	hdr := map[string]any{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	signingInput := b64url(hb) + "." + b64url(cb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + b64url(sig)
}

func rsaJWKS(kid string, pub *rsa.PublicKey) string {
	set := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": b64url(pub.N.Bytes()),
		"e": b64url(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	b, _ := json.Marshal(set)
	return string(b)
}

func ecJWKS(kid string, pub *ecdsa.PublicKey) string {
	set := map[string]any{"keys": []map[string]any{{
		"kty": "EC", "use": "sig", "crv": "P-256", "kid": kid,
		"x": b64url(pub.X.Bytes()),
		"y": b64url(pub.Y.Bytes()),
	}}}
	b, _ := json.Marshal(set)
	return string(b)
}

func validClaims() map[string]any {
	return map[string]any{"sub": "user-1", "exp": float64(time.Now().Add(time.Hour).Unix())}
}

func TestParseJWKS_RSAAndEC(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	keys, err := parseJWKS([]byte(rsaJWKS("r1", &rsaKey.PublicKey)))
	if err != nil {
		t.Fatalf("parse RSA JWKS: %v", err)
	}
	if _, ok := keys["r1"].(*rsa.PublicKey); !ok {
		t.Fatalf("expected *rsa.PublicKey for r1, got %T", keys["r1"])
	}

	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecKeys, err := parseJWKS([]byte(ecJWKS("e1", &ecKey.PublicKey)))
	if err != nil {
		t.Fatalf("parse EC JWKS: %v", err)
	}
	if _, ok := ecKeys["e1"].(*ecdsa.PublicKey); !ok {
		t.Fatalf("expected *ecdsa.PublicKey for e1, got %T", ecKeys["e1"])
	}
}

func TestJWKSCache_ResolvesAndVerifies(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rsaJWKS("k1", &key.PublicKey)))
	}))
	defer ts.Close()

	cache := newJWKSCache(ts.URL)
	token := signRS256(t, key, "k1", validClaims())

	claims, err := parseJWT(token, "", cache.key)
	if err != nil {
		t.Fatalf("parseJWT via JWKS: %v", err)
	}
	if claims["sub"] != "user-1" {
		t.Errorf("sub claim: got %v, want user-1", claims["sub"])
	}
}

func TestJWKSCache_UnknownKidRejected(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rsaJWKS("k1", &key.PublicKey)))
	}))
	defer ts.Close()

	cache := newJWKSCache(ts.URL)
	// Token signed by the same key but advertising a kid the JWKS doesn't contain.
	token := signRS256(t, key, "other", validClaims())
	if _, err := parseJWT(token, "", cache.key); err == nil {
		t.Fatal("token with an unknown kid must be rejected")
	}
}

func TestJWKSCache_RefetchOnKidMiss(t *testing.T) {
	key1, _ := rsa.GenerateKey(rand.Reader, 2048)
	key2, _ := rsa.GenerateKey(rand.Reader, 2048)
	var rotated atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rotated.Load() {
			_, _ = w.Write([]byte(rsaJWKS("k2", &key2.PublicKey)))
			return
		}
		_, _ = w.Write([]byte(rsaJWKS("k1", &key1.PublicKey)))
	}))
	defer ts.Close()

	cache := newJWKSCache(ts.URL)
	cache.minRefetch = 0 // allow immediate refetch on miss for the test

	// First key resolves.
	if _, err := parseJWT(signRS256(t, key1, "k1", validClaims()), "", cache.key); err != nil {
		t.Fatalf("k1 should resolve: %v", err)
	}

	// Issuer rotates to k2. A token with the new kid must trigger a refetch and verify.
	rotated.Store(true)
	if _, err := parseJWT(signRS256(t, key2, "k2", validClaims()), "", cache.key); err != nil {
		t.Fatalf("k2 should resolve after rotation refetch: %v", err)
	}
}

// The static-key path (JWTAuth) must keep working after the resolver refactor.
func TestParseJWT_StaticPathsStillWork(t *testing.T) {
	t.Run("hs256", func(t *testing.T) {
		hb, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT"})
		cb, _ := json.Marshal(validClaims())
		signingInput := b64url(hb) + "." + b64url(cb)
		mac := hmac.New(sha256.New, []byte("topsecret"))
		mac.Write([]byte(signingInput))
		token := signingInput + "." + b64url(mac.Sum(nil))
		if _, err := parseJWT(token, "topsecret", staticResolver(nil)); err != nil {
			t.Fatalf("HS256 static verify: %v", err)
		}
	})

	t.Run("rs256", func(t *testing.T) {
		key, _ := rsa.GenerateKey(rand.Reader, 2048)
		token := signRS256(t, key, "", validClaims())
		if _, err := parseJWT(token, "", staticResolver(&key.PublicKey)); err != nil {
			t.Fatalf("RS256 static verify: %v", err)
		}
	})
}
