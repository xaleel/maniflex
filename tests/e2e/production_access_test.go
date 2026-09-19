package e2e

// Audit AUTH-6: ValidateProduction counted any Pipeline.Auth middleware as an
// access decision, so auth.CSRF() or auth.AllowAnonymous() on its own — with no
// authenticator anywhere — passed the production audit while every route was
// open. Neither decides who may call anything: CSRF compares a cookie to a
// header, and AllowAnonymous only leaves a note for an authenticator. They no
// longer count; everything else still does, including middleware an app writes.
//
//	go test ./tests/e2e/... -run TestProductionAccess

import (
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
)

type prodAccessDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"searchable"`
}

type prodAccessOther struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

const prodAccessSecret = "production-access-secret-at-least-32b"

// prodAccessServer is otherwise production-ready, so the access check is the
// only thing left to decide the outcome.
func prodAccessServer(register func(s *maniflex.Server), models ...any) *maniflex.Server {
	s := maniflex.New(maniflex.Config{
		Strict: true, DisableAutoMigrate: true,
		QueryTimeout: 5 * time.Second, MaxConcurrentRequests: 64,
	})
	if len(models) == 0 {
		models = []any{prodAccessDoc{}}
	}
	for _, m := range models {
		s.MustRegister(m)
	}
	register(s)
	return s
}

func TestProductionAccess_NonDecidingMiddlewareAloneFails(t *testing.T) {
	t.Parallel()
	for name, mw := range map[string]maniflex.MiddlewareFunc{
		"CSRF":           auth.CSRF(),
		"AllowAnonymous": auth.AllowAnonymous(),
	} {
		t.Run(name, func(t *testing.T) {
			err := prodAccessServer(func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(mw)
			}).ValidateProduction()
			if err == nil {
				t.Fatalf("%s alone passed the production audit with every route open", name)
			}
			msg := err.Error()
			if !strings.Contains(msg, `model "prodAccessDoc"`) || !strings.Contains(msg, "access decision") {
				t.Errorf("wrong issue reported:\n%s", msg)
			}
			// A route with middleware on it reported as uncovered needs saying why,
			// or the reader assumes the audit is wrong.
			if !strings.Contains(msg, "neither decides who may call a route") {
				t.Errorf("issue does not explain why the registered middleware does not count:\n%s", msg)
			}
		})
	}
}

// The mark follows the constructor, not one instance: a second CSRF with its own
// options is skipped just the same.
func TestProductionAccess_EveryCSRFInstanceIsSkipped(t *testing.T) {
	t.Parallel()
	err := prodAccessServer(func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF())
		s.Pipeline.Auth.Register(auth.CSRF(auth.CSRFOptions{HeaderName: "X-Other-CSRF"}))
	}).ValidateProduction()
	if err == nil {
		t.Fatal("two CSRF instances together passed the audit")
	}
}

// Skipping is per route: an unscoped CSRF does not paper over the model the
// authenticator was not registered for.
func TestProductionAccess_CSRFDoesNotCoverWhatTheAuthenticatorMissed(t *testing.T) {
	t.Parallel()
	err := prodAccessServer(func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.CSRF())
		s.Pipeline.Auth.Register(auth.JWTAuth(prodAccessSecret), maniflex.ForModel("prodAccessDoc"))
	}, prodAccessDoc{}, prodAccessOther{}).ValidateProduction()
	if err == nil {
		t.Fatal("prodAccessOther has only CSRF on it and passed")
	}
	msg := err.Error()
	if !strings.Contains(msg, `model "prodAccessOther"`) {
		t.Errorf("the uncovered model is not named:\n%s", msg)
	}
	if strings.Contains(msg, `model "prodAccessDoc"`) {
		t.Errorf("the model the authenticator covers was reported too:\n%s", msg)
	}
}

func TestProductionAccess_ActionsAndSearchAreHeldToTheSameRule(t *testing.T) {
	t.Parallel()
	err := prodAccessServer(func(s *maniflex.Server) {
		// The model's routes are declared public, which leaves the action and
		// global search to be decided — by CSRF and AllowAnonymous, which don't.
		s.AllowPublic(maniflex.ForModel("prodAccessDoc"))
		s.Pipeline.Auth.Register(auth.CSRF())
		s.Pipeline.Auth.Register(auth.AllowAnonymous())
		s.Action(maniflex.ActionConfig{
			Method: "POST", Path: "/reindex",
			Handler: func(*maniflex.ServerContext) error { return nil },
		})
		s.EnableGlobalSearch(maniflex.GlobalSearchConfig{MaxLimit: 10})
	}).ValidateProduction()
	if err == nil {
		t.Fatal("an action and global search guarded only by CSRF and AllowAnonymous passed")
	}
	msg := err.Error()
	for _, want := range []string{"action POST /reindex", "global search", "neither decides who may call a route"} {
		if !strings.Contains(msg, want) {
			t.Errorf("issue missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, `model "prodAccessDoc"`) {
		t.Errorf("the model declared public was reported:\n%s", msg)
	}
}

// Must still work. Each of these makes a real decision, or is middleware the
// audit cannot see inside — which has always counted and still does.
func TestProductionAccess_RealConfigurationsStillPass(t *testing.T) {
	t.Parallel()
	for name, register := range map[string]func(s *maniflex.Server){
		"JWTAuth with CSRF": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.CSRF())
			s.Pipeline.Auth.Register(auth.JWTAuth(prodAccessSecret))
		},
		"AllowAnonymous exempting reads from JWTAuth": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.AllowAnonymous(),
				maniflex.ForOperation(maniflex.OpList, maniflex.OpRead))
			s.Pipeline.Auth.Register(auth.JWTAuth(prodAccessSecret))
		},
		"CSRF with an explicit AllowPublic": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.CSRF())
			s.AllowPublic()
		},
		// Reads explicitly public by name, writes refused without a principal.
		"AllowPublicRead alone": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.AllowPublicRead())
		},
		// The audit cannot see inside an app's own middleware, and a passthrough
		// standing in for it has always been accepted — production_test.go's
		// productionNoopAuth relies on it.
		"an app's own middleware": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				return next()
			})
		},
		// Which includes one that wraps CSRF: it has code of its own.
		"an app's own wrapper around CSRF": func(s *maniflex.Server) {
			csrf := auth.CSRF()
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				return csrf(ctx, next)
			})
		},
		// Deliberately still counted (a decided trade-off, not an oversight):
		// BlockOperation refuses the operations it names, and one closure serves
		// every instance, so the audit cannot tell which operations a given one
		// blocks. Counting it leaves "BlockOperation(OpDelete) alone" passing;
		// not counting it would flag every write a public read-only model blocks.
		"BlockOperation alone": func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpDelete))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := prodAccessServer(register).ValidateProduction(); err != nil {
				t.Errorf("a real access decision was refused:\n%v", err)
			}
		})
	}
}
