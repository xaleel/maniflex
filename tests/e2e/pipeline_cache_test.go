package e2e

// The six step chains are composed once per (model, operation) and cached when the
// router is built, instead of being rebuilt on every request (PERF-2). Caching is
// only correct if the key is the whole input to the composition — get the key wrong
// and one model's chain is served to another, which is a silent authorisation-shaped
// bug, not a slow one. These pin the scoping across a cache fill AND a cache hit,
// and the registration window that makes the cache safe to hold at all.

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestPipelineCache_KeepsModelAndOperationScoping(t *testing.T) {
	t.Parallel()

	var userLists atomic.Int32
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}, testutil.Post{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					userLists.Add(1)
					return next()
				},
				maniflex.ForModel("User"),
				maniflex.ForOperation(maniflex.OpList),
				maniflex.WithName("count-user-lists"),
			)
		},
	})
	uid := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "admin"))

	// Two identical requests: the first fills the cache entry, the second is served
	// from it. The middleware has to run both times.
	srv.GET("/users").AssertStatus(http.StatusOK)
	srv.GET("/users").AssertStatus(http.StatusOK)
	if got := userLists.Load(); got != 2 {
		t.Fatalf("middleware ran %d times over two matching requests, want 2 — "+
			"a cached chain must still carry the middleware it was composed with", got)
	}

	// Same model, different operation: must NOT reuse the list chain.
	srv.GET("/users/" + uid).AssertStatus(http.StatusOK)
	if got := userLists.Load(); got != 2 {
		t.Errorf("a read was served the list chain (count went to %d): the cache key "+
			"must include the operation", got)
	}

	// Same operation, different model: must NOT reuse the User chain.
	srv.GET("/posts").AssertStatus(http.StatusOK)
	if got := userLists.Load(); got != 2 {
		t.Errorf("a Post list was served the User chain (count went to %d): the cache "+
			"key must include the model", got)
	}
}

// A Replace middleware is the sharpest version of the same question: if the cached
// chain leaked, another model's default step would be swapped out.
func TestPipelineCache_ReplaceStaysScopedToItsModel(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}, testutil.Post{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusTeapot, "REPLACED", "the User list DB step was replaced")
					return nil
				},
				maniflex.ForModel("User"),
				maniflex.ForOperation(maniflex.OpList),
				maniflex.AtPosition(maniflex.Replace),
			)
		},
	})

	for range 2 { // fill, then hit the cache
		srv.GET("/users").AssertStatus(http.StatusTeapot)
	}
	// Posts must still reach the real DB step.
	srv.GET("/posts").AssertStatus(http.StatusOK)
}

// Registering after the router is built used to append to a slice that live requests
// were reading — a data race whose payoff was a middleware that applied to some
// requests and not others. The window is now closed explicitly.
func TestPipelineRegister_AfterHandlerPanics(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(testutil.User{})
	_ = srv.Handler() // builds the router → freezes the pipeline

	for _, step := range []struct {
		name string
		reg  *maniflex.StepRegistry
	}{
		{"Auth", srv.Pipeline.Auth},
		{"Service", srv.Pipeline.Service},
		{"DB", srv.Pipeline.DB},
		{"Response", srv.Pipeline.Response},
	} {
		t.Run(step.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Pipeline.%s.Register after Handler() was accepted — it would "+
						"mutate the middleware slice live requests read, and could never be "+
						"applied consistently", step.name)
				}
			}()
			step.reg.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				return next()
			})
		})
	}
}

// Registering before Handler() is the supported path and must stay silent.
func TestPipelineRegister_BeforeHandlerIsFine(t *testing.T) {
	t.Parallel()

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(testutil.User{})
	srv.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
		return next()
	})
	_ = srv.Handler()
}
