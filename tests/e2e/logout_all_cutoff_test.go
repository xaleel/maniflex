package e2e

// Audit AUTH-7: a "log out everywhere" cutoff carries nanoseconds, a token's
// iat is a whole second, and each store rounded that cutoff its own way — so the
// same application refused or accepted a token minted in the logout's second
// depending on which blocklist it used. Where it refused, the replacement token
// an app mints straight after a password change was dead on arrival, every time.
//
// The second is now refused everywhere, and LogoutAll answers only once it has
// passed, so a token minted after the response can be placed after the cutoff.
//
//	go test ./tests/e2e/... -run TestLogoutAllCutoff

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const cutoffSecret = "logout-all-cutoff-secret-at-least-32b"

type cutoffDoc struct{ maniflex.BaseModel }

// mintCutoffToken signs an HS256 token with exactly the claims given, so a test
// can choose an iat to the fraction.
func mintCutoffToken(claims map[string]any) map[string]string {
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	signingInput := header + "." + enc(body)
	mac := hmac.New(sha256.New, []byte(cutoffSecret))
	mac.Write([]byte(signingInput))
	return map[string]string{"Authorization": "Bearer " + signingInput + "." + enc(mac.Sum(nil))}
}

func cutoffServer(t *testing.T, rev auth.Revoker) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{cutoffDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(cutoffSecret, auth.JWTOptions{Revoker: rev}))
			s.Action(auth.LogoutAll(rev, "/logout-all", time.Hour))
		},
	})
}

// Every store must answer the same, whichever way it stores the cutoff: the
// logout's own second is refused, the next one is not.
func TestLogoutAllCutoff_StoresAgreeOnTheAmbiguousSecond(t *testing.T) {
	t.Parallel()
	sqlRev, _ := newSQLRevoker(t, "cutoff_")
	for name, rev := range map[string]auth.Revoker{
		"memory": auth.NewMemoryRevoker(),
		"sql":    sqlRev,
	} {
		t.Run(name, func(t *testing.T) {
			srv := cutoffServer(t, rev)
			base := time.Now().Truncate(time.Second).Add(-10 * time.Second)
			user := "user-" + name
			if err := rev.RevokeUser(context.Background(), user,
				base.Add(500*time.Millisecond), time.Now().Add(time.Hour)); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			exp := time.Now().Add(time.Hour).Unix()
			get := func(jti string, iat any) int {
				return srv.GET("/cutoff_docs", mintCutoffToken(map[string]any{
					"sub": user, "jti": jti, "iat": iat, "exp": exp})).Status
			}
			// Same second as the cutoff: indistinguishable from one minted just
			// before the logout, so refused.
			if got := get("same", base.Unix()); got != http.StatusUnauthorized {
				t.Errorf("token from the cutoff's own second: %d, want 401", got)
			}
			// Must still work: a later token is untouched, or "log out
			// everywhere" would mean "log out forever".
			if got := get("next", base.Unix()+1); got != http.StatusOK {
				t.Errorf("token from the next second: %d, want 200", got)
			}
			// Must still work: anything genuinely older is still revoked.
			if got := get("older", base.Unix()-60); got != http.StatusUnauthorized {
				t.Errorf("token a minute older: %d, want 401", got)
			}
		})
	}
}

// The flow the bug broke: log out everywhere, then issue the session a fresh
// token. It used to be refused every time.
func TestLogoutAllCutoff_ReplacementTokenIsAccepted(t *testing.T) {
	t.Parallel()
	srv := cutoffServer(t, auth.NewMemoryRevoker())
	now := time.Now()
	old := mintCutoffToken(map[string]any{
		"sub": "alice", "jti": "old",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	srv.POST("/logout-all", nil, old).AssertStatus(http.StatusNoContent)

	// Must still work: the token that asked for the logout is itself revoked.
	srv.GET("/cutoff_docs", old).AssertStatus(http.StatusUnauthorized)

	fresh := time.Now()
	replacement := mintCutoffToken(map[string]any{
		"sub": "alice", "jti": "fresh",
		"iat": fresh.Unix(), "exp": fresh.Add(time.Hour).Unix(),
	})
	srv.GET("/cutoff_docs", replacement).AssertStatus(http.StatusOK)
}

// What makes the replacement safe is that the response is withheld until the
// ambiguous second is over, so this asserts the boundary rather than the
// symptom — a fix that accepted the second instead would pass the test above.
func TestLogoutAllCutoff_ResponseWaitsForTheNextSecond(t *testing.T) {
	t.Parallel()
	srv := cutoffServer(t, auth.NewMemoryRevoker())
	now := time.Now()
	token := mintCutoffToken(map[string]any{
		"sub": "bob", "jti": "j", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})

	before := time.Now()
	srv.POST("/logout-all", nil, token).AssertStatus(http.StatusNoContent)
	after := time.Now()

	if before.Nanosecond() == 0 {
		t.Skip("the cutoff landed exactly on a second; there was nothing to wait out")
	}
	if !after.After(before.Truncate(time.Second).Add(time.Second)) {
		t.Errorf("answered at %s, within the same second as the cutoff at %s",
			after.Format("05.000"), before.Format("05.000"))
	}
	if waited := after.Sub(before); waited > 2*time.Second {
		t.Errorf("waited %v, which is more than the one ambiguous second", waited)
	}
}

// An issuer that stamps sub-second iat needs no wait at all: the fraction says
// which side of the cutoff the token falls on. The in-memory store keeps the
// cutoff to the nanosecond; the SQL store's column holds whole seconds, so it
// cannot, and refuses the second either way.
func TestLogoutAllCutoff_FractionalIatIsPlacedExactly(t *testing.T) {
	t.Parallel()
	rev := auth.NewMemoryRevoker()
	srv := cutoffServer(t, rev)
	base := time.Now().Truncate(time.Second).Add(-10 * time.Second)
	if err := rev.RevokeUser(context.Background(), "carol",
		base.Add(500*time.Millisecond), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	get := func(jti string, iat float64) int {
		return srv.GET("/cutoff_docs", mintCutoffToken(map[string]any{
			"sub": "carol", "jti": jti, "iat": iat, "exp": exp})).Status
	}
	if got := get("after", float64(base.Unix())+0.7); got != http.StatusOK {
		t.Errorf("iat .700, after a .500 cutoff: %d, want 200", got)
	}
	if got := get("before", float64(base.Unix())+0.3); got != http.StatusUnauthorized {
		t.Errorf("iat .300, before a .500 cutoff: %d, want 401", got)
	}
}
