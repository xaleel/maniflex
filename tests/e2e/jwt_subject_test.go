package e2e_test

// Audit AUTH-5: JWTAuth read the user id with a bare .(string) and discarded the
// failure, so a token whose sub was a JSON number — or which had none — was
// accepted with UserID "". Everything keyed on the user id then saw every such
// caller as one person: under RequireOwner, user 43 read and rewrote user 42's
// records. The tenant claim had the identical bug.
//
//	go test ./tests/e2e/... -run 'TestJWTSubject|TestOwnerGuard'

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/middleware/service"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const subjectSecret = "subject-test-secret-at-least-32-bytes!"

type subjectNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id"`
}

// subjectOpen has no owner scoping, so what reaches it shows what JWTAuth
// itself accepts.
type subjectOpen struct{ maniflex.BaseModel }

func withClaims(t *testing.T, claims map[string]any) map[string]string {
	t.Helper()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	return map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, subjectSecret, claims)}
}

func subjectServer(t *testing.T, opt auth.JWTOptions) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{subjectNote{}, subjectOpen{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(subjectSecret, opt))
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"),
				maniflex.ForModel("subjectNote"))
		},
	})
}

func TestJWTSubject_NumericSubjectsAreDistinctUsers(t *testing.T) {
	t.Parallel()
	srv := subjectServer(t, auth.JWTOptions{})
	user42 := withClaims(t, map[string]any{"sub": 42})
	user43 := withClaims(t, map[string]any{"sub": 43})

	created := srv.POST("/subject_notes", map[string]any{"title": "42's"}, user42)
	created.AssertStatus(http.StatusCreated)
	if got := created.Data()["owner_id"]; got != "42" {
		t.Fatalf("owner_id %v, want \"42\"", got)
	}
	id := created.ID()

	srv.GET("/subject_notes/"+id, user43).AssertStatus(http.StatusNotFound)
	srv.PATCH("/subject_notes/"+id, map[string]any{"title": "43 was here"}, user43).
		AssertStatus(http.StatusNotFound)
	// Must still work: the owner.
	srv.GET("/subject_notes/"+id, user42).AssertStatus(http.StatusOK)
}

// float64 holds integers exactly only to 2^53. Formatting the decoded claim —
// the obvious repair — would give these two users the same id.
func TestJWTSubject_LargeNumericSubjectsStayDistinct(t *testing.T) {
	t.Parallel()
	srv := subjectServer(t, auth.JWTOptions{})
	upper := withClaims(t, map[string]any{"sub": int64(9007199254740993)})
	lower := withClaims(t, map[string]any{"sub": int64(9007199254740992)})

	created := srv.POST("/subject_notes", map[string]any{"title": "t"}, upper)
	created.AssertStatus(http.StatusCreated)
	if got := created.Data()["owner_id"]; got != "9007199254740993" {
		t.Fatalf("owner_id %v, want the exact id 9007199254740993", got)
	}
	srv.GET("/subject_notes/"+created.ID(), lower).AssertStatus(http.StatusNotFound)
	srv.GET("/subject_notes/"+created.ID(), upper).AssertStatus(http.StatusOK)
}

func TestJWTSubject_MissingSubjectRefused(t *testing.T) {
	t.Parallel()
	srv := subjectServer(t, auth.JWTOptions{})
	for name, claims := range map[string]map[string]any{
		"absent": {},
		"empty":  {"sub": ""},
		"null":   {"sub": nil},
	} {
		// On the open route, so the refusal is JWTAuth's and not RequireOwner's.
		resp := srv.GET("/subject_opens", withClaims(t, claims))
		resp.AssertStatus(http.StatusUnauthorized)
		if code := jsonErrCode(resp.Body); code != "TOKEN_MISSING_SUBJECT" {
			t.Errorf("%s sub: code %q, want TOKEN_MISSING_SUBJECT", name, code)
		}
	}
}

// AllowNoSubject restores acceptance — and the owner middlewares still refuse
// such a principal, since it has nothing to own records by.
func TestJWTSubject_AllowNoSubjectOptIn(t *testing.T) {
	t.Parallel()
	srv := subjectServer(t, auth.JWTOptions{AllowNoSubject: true})
	subjectless := withClaims(t, map[string]any{})

	srv.GET("/subject_opens", subjectless).AssertStatus(http.StatusOK)
	srv.POST("/subject_notes", map[string]any{"title": "t"}, subjectless).
		AssertStatus(http.StatusUnauthorized)

	// Must still work: adminRoles bypass RequireOwner before the guard, which
	// never needed the id — a subject-less service token with an admin role.
	admin := withClaims(t, map[string]any{"roles": []string{"admin"}})
	srv.POST("/subject_notes", map[string]any{"title": "t"}, admin).
		AssertStatus(http.StatusCreated)
}

func TestJWTSubject_NonIntegerSubjectRefused(t *testing.T) {
	t.Parallel()
	srv := subjectServer(t, auth.JWTOptions{})
	for name, sub := range map[string]any{
		"fraction": 4.5,
		"boolean":  true,
		"object":   map[string]any{"id": 1},
	} {
		resp := srv.GET("/subject_opens", withClaims(t, map[string]any{"sub": sub}))
		resp.AssertStatus(http.StatusUnauthorized)
		if code := jsonErrCode(resp.Body); code != "INVALID_TOKEN" {
			t.Errorf("%s sub: code %q, want INVALID_TOKEN", name, code)
		}
	}
}

func TestJWTSubject_NumericTenantClaim(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen *maniflex.AuthInfo
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{subjectOpen{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(subjectSecret, auth.JWTOptions{TenantClaim: "tenant_id"}))
			s.Pipeline.DB.Register(captureAuthMiddleware(&mu, &seen), maniflex.AtPosition(maniflex.Before))
		},
	})
	srv.GET("/subject_opens", withClaims(t, map[string]any{"sub": "u", "tenant_id": 7})).
		AssertStatus(http.StatusOK)
	mu.Lock()
	defer mu.Unlock()
	if seen == nil || seen.TenantID != "7" {
		t.Fatalf("TenantID %+v, want \"7\"", seen)
	}
}

// The per-user revocation cutoff is keyed on the user id and skipped when it is
// empty, and LogoutAll refuses an empty id outright — so for a numeric-sub user
// "log out everywhere" answered 401 and revoked nothing.
func TestJWTSubject_LogoutAllReachesNumericSubject(t *testing.T) {
	t.Parallel()
	srv := revocationServer(t, subjectSecret, auth.NewMemoryRevoker())
	now := time.Now()
	token := func(jti string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + makeJWTClaims(t, subjectSecret, map[string]any{
			"sub": 42, "jti": jti, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})}
	}
	phone, laptop := token("jti-phone"), token("jti-laptop")

	srv.POST("/logout-all", nil, phone).AssertStatus(http.StatusNoContent)
	srv.GET("/revokable_models", phone).AssertStatus(http.StatusUnauthorized)
	srv.GET("/revokable_models", laptop).AssertStatus(http.StatusUnauthorized)
}

// The owner guards apply to any authenticator, not only JWTAuth: an APIKeyEntry
// or a custom middleware can install a principal with no user id just as well.
func TestOwnerGuard_EmptyUserIDRefused(t *testing.T) {
	t.Parallel()
	emptyPrincipal := func(ctx *maniflex.ServerContext, next func() error) error {
		ctx.Auth = &maniflex.AuthInfo{Roles: []string{"member"}}
		return next()
	}
	for name, guard := range map[string]func(s *maniflex.Server){
		"RequireOwner": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id"))
		},
		"OwnerScope": func(s *maniflex.Server) {
			s.Pipeline.Service.Register(service.OwnerScope("owner_id"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{subjectNote{}},
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.Auth.Register(emptyPrincipal)
					guard(s)
				},
			})
			srv.POST("/subject_notes", map[string]any{"title": "t"}).
				AssertStatus(http.StatusUnauthorized)
		})
	}
}
