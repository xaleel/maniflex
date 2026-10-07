package e2e

// Audit AUTH-14: a JWKS fetch failure was reported as 401 INVALID_TOKEN
// carrying the fetch error's own text.
//
// That was wrong twice over. The message named the issuer's internal host,
// port, path and failure class to a caller who had not authenticated - the
// token is never verified on this path, because the key fetch happens first, so
// any syntactically valid bearer token reached it. And a 401 tells every
// healthy client to re-authenticate during an outage none of them caused, which
// this codebase had already settled for the revocation store:
// CodeRevocationUnavailable is a 503 because "a 401 here would send every
// healthy client into a re-login storm during a store outage".
//
// Now a fetch failure is 503 JWKS_UNAVAILABLE. Abort masks a 5xx message with
// generic status text by itself and keeps the code, so the client still learns
// which of the two failures happened while the cause goes to the log.
//
// The row that must not move: a *successful* fetch whose set lacks the token's
// key is still 401. That is a statement about the token, not about the issuer.
//
//	go test ./tests/e2e/... -run TestJWKSUnavailable

import (
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type jwksUnavailDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

func jwksUnavailServer(t *testing.T, url string, logs *lockedLog) *testutil.Server {
	t.Helper()
	opts := testutil.Options{
		Models: []any{jwksUnavailDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWKSAuth(url))
		},
	}
	if logs != nil {
		opts.Logger = newLockedLogger(logs)
	}
	return testutil.NewServer(t, opts)
}

// THE FINDING. An unreachable issuer answers 503 with nothing about the issuer
// in the body.
func TestJWKSUnavailable_UnreachableIssuerIs503WithNoDetail(t *testing.T) {
	t.Parallel()
	key := mustRSAKey(t)
	tok := makeAsymJWT(t, "RS256", key, "u1", nil, time.Hour)

	for _, tc := range []struct{ name, url string }{
		{"connection refused", "http://127.0.0.1:1/.well-known/jwks.json"},
		{"unresolvable internal host", "https://idp.internal.example:8443/.well-known/jwks.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := jwksUnavailServer(t, tc.url, nil).
				GET("/jwks_unavail_docs", map[string]string{"Authorization": "Bearer " + tok})
			resp.AssertStatus(http.StatusServiceUnavailable)

			body := string(resp.Body)
			if !strings.Contains(body, auth.CodeJWKSUnavailable) {
				t.Errorf("the code must survive the 5xx mask so a client can tell "+
					"'retry later' from 're-authenticate'; got %s", body)
			}
			// Nothing about where the issuer lives or why it failed.
			for _, leak := range []string{
				"idp.internal", "127.0.0.1", "8443", "well-known", "jwks.json",
				"dial tcp", "connection refused", "no such host", "lookup",
			} {
				if strings.Contains(body, leak) {
					t.Errorf("response leaks %q about the issuer: %s", leak, body)
				}
			}
		})
	}
}

// The cause is not discarded, only redirected: an operator needs the host and
// the failure class, which is exactly what the client must not get.
func TestJWKSUnavailable_CauseIsLogged(t *testing.T) {
	t.Parallel()
	key := mustRSAKey(t)
	tok := makeAsymJWT(t, "RS256", key, "u1", nil, time.Hour)
	logs := &lockedLog{}

	jwksUnavailServer(t, "https://idp.internal.example:8443/.well-known/jwks.json", logs).
		GET("/jwks_unavail_docs", map[string]string{"Authorization": "Bearer " + tok}).
		AssertStatus(http.StatusServiceUnavailable)

	got := logs.String()
	if !strings.Contains(got, "JWK Set unavailable") {
		t.Errorf("the failure is not reported to the operator at all; logs:\n%s", got)
	}
	if !strings.Contains(got, "idp.internal.example") {
		t.Errorf("the log must name the issuer that could not be reached, or the "+
			"503 is undiagnosable; logs:\n%s", got)
	}
}

// MUST STILL WORK, and the distinction the whole fix rests on: a reachable
// issuer whose set simply lacks this token's key is a statement about the
// token, so it stays 401.
func TestJWKSUnavailable_ReachableIssuerMissingTheKeyIsStill401(t *testing.T) {
	t.Parallel()
	served := mustRSAKey(t)
	other := mustRSAKey(t)

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rsaJWKSBody(t, &served.PublicKey)))
	}))
	defer idp.Close()

	// Signed by a key the issuer does not publish.
	tok := makeAsymJWT(t, "RS256", other, "u1", nil, time.Hour)
	resp := jwksUnavailServer(t, idp.URL, nil).
		GET("/jwks_unavail_docs", map[string]string{"Authorization": "Bearer " + tok})
	resp.AssertStatus(http.StatusUnauthorized)
	if strings.Contains(string(resp.Body), auth.CodeJWKSUnavailable) {
		t.Errorf("a reachable issuer must not report JWKS_UNAVAILABLE: %s", resp.Body)
	}
}

// MUST STILL WORK: a valid token against a reachable issuer is served, so the
// new branch has not swallowed the success path.
func TestJWKSUnavailable_ValidTokenStillPasses(t *testing.T) {
	t.Parallel()
	key := mustRSAKey(t)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rsaJWKSBody(t, &key.PublicKey)))
	}))
	defer idp.Close()

	tok := makeAsymJWT(t, "RS256", key, "u1", nil, time.Hour)
	jwksUnavailServer(t, idp.URL, nil).
		GET("/jwks_unavail_docs", map[string]string{"Authorization": "Bearer " + tok}).
		AssertStatus(http.StatusOK)
}

// MUST STILL WORK: an issuer that answers but not with a JWK Set is still the
// issuer's problem, so it is 503 too - and the status code it sent stays out of
// the response, since that is a detail about the IdP.
func TestJWKSUnavailable_IssuerErrorStatusIsAlso503(t *testing.T) {
	t.Parallel()
	key := mustRSAKey(t)
	tok := makeAsymJWT(t, "RS256", key, "u1", nil, time.Hour)

	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		resp := jwksUnavailServer(t, idp.URL, nil).
			GET("/jwks_unavail_docs", map[string]string{"Authorization": "Bearer " + tok})
		resp.AssertStatus(http.StatusServiceUnavailable)
		if strings.Contains(string(resp.Body), "404") || strings.Contains(string(resp.Body), "500") {
			t.Errorf("the issuer's own status leaked into the response: %s", resp.Body)
		}
		idp.Close()
	}
}

// MUST STILL WORK (AUTH-1): a cached key is preferred over an outage, so an
// issuer going down does not log out the users whose key is already held. This
// is the reason key() only reports unavailable when it has nothing to fall back
// on, and it keeps the 503 from becoming a self-inflicted outage.
func TestJWKSUnavailable_CachedKeySurvivesAnOutage(t *testing.T) {
	t.Parallel()
	key := mustRSAKey(t)
	var down atomic.Bool
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rsaJWKSBody(t, &key.PublicKey)))
	}))
	defer idp.Close()

	srv := jwksUnavailServer(t, idp.URL, nil)
	tok := makeAsymJWT(t, "RS256", key, "u1", nil, time.Hour)
	hdr := map[string]string{"Authorization": "Bearer " + tok}

	srv.GET("/jwks_unavail_docs", hdr).AssertStatus(http.StatusOK) // warms the cache
	down.Store(true)
	srv.GET("/jwks_unavail_docs", hdr).AssertStatus(http.StatusOK)
}

// newLockedLogger wires the server's logger into a lockedLog (declared in
// read_audit_background_test.go) so a test can assert on what was reported.
func newLockedLogger(dst *lockedLog) *slog.Logger {
	return slog.New(slog.NewTextHandler(dst, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// rsaJWKSBody renders a one-key JWK Set for pub. No "kid" is set, so the key is
// indexed under "" and matches a token whose header carries no kid either -
// which is the shape makeAsymJWT produces.
func rsaJWKSBody(t *testing.T, pub *rsa.PublicKey) string {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	return fmt.Sprintf(`{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","n":%q,"e":%q}]}`, n, e)
}
