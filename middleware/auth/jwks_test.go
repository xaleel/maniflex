package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

// ── JWKS fetch stampede (audit AUTH-1) ───────────────────────────────────────

// jwksServer returns a JWK Set server that counts requests, serves the given
// kid, and can be switched to failing. The delay is what makes a concurrency
// test meaningful: without it the first caller finishes before the rest arrive
// and a serialised implementation would pass too.
func jwksServer(t *testing.T, kid string, delay time.Duration) (*httptest.Server, *rsa.PrivateKey, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var hits atomic.Int64
	var failing atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(delay)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(rsaJWKS(kid, &key.PublicKey)))
	}))
	t.Cleanup(ts.Close)
	return ts, key, &hits, &failing
}

// callKeyConcurrently fires n simultaneous key() lookups and returns how many
// upstream fetches they caused.
func callKeyConcurrently(c *jwksCache, hits *atomic.Int64, n int, kid string) int64 {
	before := hits.Load()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { _, _ = c.key(kid, "RS256") })
	}
	wg.Wait()
	return hits.Load() - before
}

// key() runs inside parseJWT *before* the signature is checked, so an
// unauthenticated client picks the kid and therefore decides whether a fetch is
// attempted. Concurrent callers must share one request: without the latch, 25
// garbage tokens meant 25 outbound connections to the issuer, each held for up
// to the client's 10s timeout — amplification against the IdP and FD exhaustion
// at home (audit AUTH-1).
func TestJWKSCacheCollapsesConcurrentFetches(t *testing.T) {
	t.Run("cache never populated", func(t *testing.T) {
		ts, _, hits, _ := jwksServer(t, "k1", 50*time.Millisecond)
		c := newJWKSCache(ts.URL)
		if got := callKeyConcurrently(c, hits, 25, "unknown"); got != 1 {
			t.Errorf("25 concurrent callers caused %d upstream fetches, want 1", got)
		}
	})

	t.Run("cache stale", func(t *testing.T) {
		ts, _, hits, _ := jwksServer(t, "k1", 50*time.Millisecond)
		c := newJWKSCache(ts.URL)
		if err := c.refresh(); err != nil {
			t.Fatalf("prime cache: %v", err)
		}
		c.mu.Lock()
		c.fetchedAt = time.Now().Add(-2 * c.ttl) // past its TTL
		c.mu.Unlock()

		// A hit on a stale set still refreshes before answering, so every caller
		// reaches refresh — they must not each reach the network.
		if got := callKeyConcurrently(c, hits, 25, "k1"); got != 1 {
			t.Errorf("25 concurrent callers over a stale set caused %d fetches, want 1", got)
		}
		if _, err := c.key("k1", "RS256"); err != nil {
			t.Errorf("k1 should resolve after the refresh: %v", err)
		}
	})
}

// A failed fetch used to record nothing that throttled the next one: the guard
// also required a non-empty, fresh cache, which is exactly what a failing
// issuer prevents. So a down IdP was re-fetched on every single request,
// sequentially as well as concurrently.
func TestJWKSCacheBacksOffWhileIssuerIsDown(t *testing.T) {
	ts, _, hits, failing := jwksServer(t, "k1", 0)
	failing.Store(true)
	c := newJWKSCache(ts.URL)
	c.minRetry = time.Minute // long enough that the test never races it

	for range 10 {
		if _, err := c.key("k1", "RS256"); err == nil {
			t.Fatal("key() must fail while the issuer is down and nothing is cached")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("10 sequential lookups against a down issuer caused %d fetches, want 1", got)
	}

	// Once the backoff has elapsed, one retry is allowed — and only one.
	c.mu.Lock()
	first := c.backoff
	c.lastAttempt = time.Now().Add(-2 * time.Minute)
	c.mu.Unlock()
	for range 10 {
		_, _ = c.key("k1", "RS256")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("after the backoff elapsed, %d total fetches, want 2", got)
	}

	// And the wait doubles, so a sustained outage costs the issuer a handful of
	// requests rather than a steady stream.
	c.mu.RLock()
	second := c.backoff
	c.mu.RUnlock()
	if first != time.Minute || second != 2*time.Minute {
		t.Errorf("backoff did not double: first %v, second %v", first, second)
	}
}

// Backing off must not make the failure unreadable. Without a remembered error
// the throttled caller fell through to the kid lookup and was told "no signing
// key for kid", which sends whoever is debugging it to the issuer's key
// rotation rather than to the fact that the issuer is unreachable.
func TestJWKSCacheReportsIssuerErrorWhileBackingOff(t *testing.T) {
	ts, _, hits, failing := jwksServer(t, "k1", 0)
	failing.Store(true)
	c := newJWKSCache(ts.URL)
	c.minRetry = time.Minute

	if _, err := c.key("k1", "RS256"); err == nil {
		t.Fatal("first lookup must fail")
	}
	_, err := c.key("k1", "RS256")
	if err == nil {
		t.Fatal("throttled lookup must still fail")
	}
	// Guard against a vacuous pass: this only exercises the remembered error if
	// the second lookup was genuinely throttled. Allowed to re-fetch, it would
	// get a fresh 500 and satisfy the assertion below for the wrong reason.
	if got := hits.Load(); got != 1 {
		t.Fatalf("second lookup was not throttled (%d fetches), so it proves nothing", got)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("throttled lookup reported %q, want the issuer's status", err)
	}
	if strings.Contains(err.Error(), "no signing key") {
		t.Errorf("throttled lookup blamed key rotation: %q", err)
	}
}

func TestJWKSCacheBackoffResetsAfterSuccess(t *testing.T) {
	ts, _, hits, failing := jwksServer(t, "k1", 0)
	failing.Store(true)
	c := newJWKSCache(ts.URL)
	c.minRetry = time.Millisecond

	if _, err := c.key("k1", "RS256"); err == nil {
		t.Fatal("first lookup must fail")
	}
	failing.Store(false)
	c.mu.Lock()
	c.lastAttempt = time.Now().Add(-time.Second) // backoff elapsed
	c.mu.Unlock()
	if _, err := c.key("k1", "RS256"); err != nil {
		t.Fatalf("lookup after recovery: %v", err)
	}

	c.mu.RLock()
	backoff, lastErr := c.backoff, c.lastErr
	c.mu.RUnlock()
	if backoff != 0 || lastErr != nil {
		t.Errorf("success left backoff %v, lastErr %v; want 0 and nil", backoff, lastErr)
	}

	// The reset must not reopen the stampede: a fresh set still throttles.
	if got := callKeyConcurrently(c, hits, 10, "unknown"); got != 0 {
		t.Errorf("unknown kid against a fresh set caused %d fetches, want 0", got)
	}
}

// The response was read with an unbounded io.ReadAll, so whoever controls the
// bytes at the JWKS URL — or anything broken between here and it — could make
// the process allocate without limit.
func TestJWKSCacheRejectsOversizedSet(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for n := 0; n <= maxJWKSBody; n += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	c := newJWKSCache(ts.URL)
	err := c.refresh()
	if err == nil {
		t.Fatal("an oversized JWK Set must be refused")
	}
	// Named, because a truncated body also fails to parse — as a JSON syntax
	// error that says nothing about why.
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized set refused for the wrong reason: %v", err)
	}
	c.mu.RLock()
	n := len(c.keys)
	c.mu.RUnlock()
	if n != 0 {
		t.Errorf("cached %d key(s) from an oversized fetch, want 0", n)
	}
}

// Must still work: a cached key of any age beats a failed refresh. This is what
// stops an IdP outage from logging out every user at the TTL boundary, and the
// backoff must not have replaced it with a cached *error*.
func TestJWKSCacheServesCachedKeyWhenIssuerFails(t *testing.T) {
	ts, key, _, failing := jwksServer(t, "k1", 0)
	c := newJWKSCache(ts.URL)
	if _, err := parseJWT(signRS256(t, key, "k1", validClaims()), "", c.key); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	failing.Store(true)
	c.mu.Lock()
	c.fetchedAt = time.Now().Add(-2 * c.ttl) // force a refresh attempt
	c.mu.Unlock()

	// The refresh fails; the cached k1 must still verify the token.
	if _, err := parseJWT(signRS256(t, key, "k1", validClaims()), "", c.key); err != nil {
		t.Fatalf("cached key must survive a failing refresh: %v", err)
	}
	// And again while throttled, which is the path the backoff added.
	if _, err := parseJWT(signRS256(t, key, "k1", validClaims()), "", c.key); err != nil {
		t.Fatalf("cached key must survive the backoff: %v", err)
	}
}

// Must still work: the one case the original guard did cover. An unknown kid
// against a fresh set is a rotation that has not happened, and refetching on
// every such token is the storm the minRefetch interval exists to stop.
func TestJWKSCacheThrottlesUnknownKidWithFreshCache(t *testing.T) {
	ts, _, hits, _ := jwksServer(t, "k1", 0)
	c := newJWKSCache(ts.URL)
	if err := c.refresh(); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	for range 20 {
		if _, err := c.key("rotated-away", "RS256"); err == nil {
			t.Fatal("an unknown kid must not resolve")
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("20 unknown-kid lookups against a fresh set caused %d fetches, want 1", got)
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

// The JWK Set is the whole root of trust for JWKSAuth: there is no shared
// secret, so whoever controls the bytes at that URL decides who is
// authenticated. Fetched over plaintext, an active network attacker swaps in
// their own public key and mints tokens with any claims they like (audit S11).
//
// Loopback is exempt rather than warned about. A local Keycloak or dex on
// http://localhost is a normal development setup, and every JWKS test in this
// file already talks to an httptest server on 127.0.0.1 — warning on that would
// make the warning noise, which is how warnings get ignored.
func TestPlaintextJWKSWarning(t *testing.T) {
	secure := []string{
		"https://issuer.example.com/.well-known/jwks.json",
		"https://localhost:8443/jwks.json",
		"http://localhost:8080/jwks.json",
		"http://127.0.0.1:8080/jwks.json",
		"http://127.0.0.2/jwks.json",
		"http://[::1]:8080/jwks.json",
	}
	for _, raw := range secure {
		if got := plaintextJWKSWarning(raw); got != "" {
			t.Errorf("plaintextJWKSWarning(%q) = %q, want no warning", raw, got)
		}
	}

	insecure := []string{
		"http://issuer.example.com/.well-known/jwks.json",
		// A private LAN address is still another host on a wire someone can sit on.
		"http://192.168.1.10/jwks.json",
		"http://10.0.0.5/jwks.json",
		// Neither scheme the fetch can use; it will fail, but say so at boot.
		"ftp://issuer.example.com/jwks.json",
		"://not a url",
		"",
	}
	for _, raw := range insecure {
		if plaintextJWKSWarning(raw) == "" {
			t.Errorf("plaintextJWKSWarning(%q) = \"\", want a warning", raw)
		}
	}
}

// captureDefaultLogger swaps slog's default for the duration of the test and
// returns what was written to it. Not parallel-safe, which is why none of the
// tests using it call t.Parallel.
func captureDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The warning has to arrive at construction. Nothing fetches the JWK Set until
// the first token is validated, so a plaintext URL is otherwise invisible until
// production traffic arrives — and then it does not fail, it succeeds.
func TestJWKSAuthWarnsAtConstructionOnPlaintextIssuer(t *testing.T) {
	buf := captureDefaultLogger(t)
	_ = JWKSAuth("http://issuer.example.com/.well-known/jwks.json")
	if !strings.Contains(buf.String(), "JWKSAuth") || !strings.Contains(buf.String(), "plaintext") {
		t.Errorf("no plaintext warning logged; got %q", buf.String())
	}
}

func TestJWKSAuthSilentOnSecureAndLoopbackIssuers(t *testing.T) {
	for _, raw := range []string{
		"https://issuer.example.com/.well-known/jwks.json",
		"http://localhost:8080/jwks.json",
	} {
		buf := captureDefaultLogger(t)
		// Audience is set so this keeps testing what it is about — that a secure
		// or loopback URL draws no *transport* warning — rather than picking up
		// the unrelated one about audience confusion (audit AUTH-8).
		_ = JWKSAuth(raw, JWTOptions{Audience: "api"})
		if buf.Len() != 0 {
			t.Errorf("JWKSAuth(%q) logged %q, want silence", raw, buf.String())
		}
	}
}

// A scheme check on the configured URL is not enough on its own. Go's default
// http.Client follows an https -> http redirect without complaint, so an
// issuer that redirects downgrades the fetch of the key material that decides
// authentication, and the configured https:// URL reads as if it did not.
func TestJWKSRedirectMayNotWeakenTransport(t *testing.T) {
	loopbackPlain, _ := url.Parse("http://127.0.0.1:9000/jwks.json")
	remotePlain, _ := url.Parse("http://issuer.example.com/jwks.json")
	secure, _ := url.Parse("https://issuer.example.com/jwks.json")

	cases := []struct {
		name    string
		from    *url.URL
		to      *url.URL
		wantErr bool
	}{
		{"https to https", secure, secure, false},
		{"https to plaintext is a downgrade", secure, remotePlain, true},
		{"https to loopback plaintext is still a downgrade", secure, loopbackPlain, true},
		{"loopback plaintext to loopback plaintext", loopbackPlain, loopbackPlain, false},
		{"loopback plaintext to a remote plaintext host", loopbackPlain, remotePlain, true},
		{"plaintext to https", remotePlain, secure, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkJWKSRedirect(tc.from, tc.to)
			if tc.wantErr && err == nil {
				t.Error("want an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}

// End to end over a real redirect, because the policy is only worth anything if
// it is wired into the client that does the fetching.
func TestJWKSCacheRefusesDowngradingRedirect(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(rsaJWKS("k1", &key.PublicKey)))
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/jwks.json", http.StatusFound)
	}))
	defer tlsSrv.Close()

	cache := newJWKSCache(tlsSrv.URL)
	// Swap only the transport, so the test trusts the server's certificate while
	// keeping the redirect policy newJWKSCache installed. Replacing the whole
	// client would test a policy this test wired itself, which is no test of the
	// constructor at all.
	cache.client.Transport = tlsSrv.Client().Transport

	err := cache.refresh()
	if err == nil {
		t.Fatal("refresh followed an https -> http redirect")
	}
	// Named explicitly: a TLS verification failure would also make refresh error
	// and would pass this test for entirely the wrong reason.
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("refresh failed for the wrong reason: %v", err)
	}
	cache.mu.RLock()
	n := len(cache.keys)
	cache.mu.RUnlock()
	if n != 0 {
		t.Errorf("cached %d key(s) from a downgraded fetch, want 0", n)
	}
}
