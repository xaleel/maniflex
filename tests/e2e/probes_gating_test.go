package e2e

// probes_gating_test.go covers Config.Probes (audit DOC-4): the probe endpoints
// bypass Pipeline.Auth by design, so this is the only way to put anything in
// front of them, or to take one off the router entirely.
//
//	Probes.Middleware        wraps every mounted probe
//	Probes.{Live,Ready,Health}.Middleware  wraps that one, after the shared chain
//	Probes.{Live,Ready,Health}.Disabled    leaves it unmounted (404 from chi)
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestProbeGating
//
// The probes' unguarded behaviour lives in probes_test.go and health_test.go.

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Defaults ──────────────────────────────────────────────────────────────────

func TestProbeGating_ZeroValueMountsAllThreePublic(t *testing.T) {
	// The zero value must not change what shipped before Config.Probes existed:
	// an operator who never touches it keeps three public probes.
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})
	for _, path := range probePaths {
		srv.GET(path).AssertStatus(http.StatusOK)
	}
}

// ── Shared chain ──────────────────────────────────────────────────────────────

func TestProbeGating_SharedMiddlewareWrapsEveryProbe(t *testing.T) {
	t.Parallel()

	t.Run("rejects_every_probe_without_the_credential", func(t *testing.T) {
		t.Parallel()
		srv := gatedProbeServer(t, maniflex.ProbesConfig{
			Middleware: []maniflex.HTTPMiddleware{probeToken("s3cret")},
		})
		for _, path := range probePaths {
			srv.GET(path).AssertStatus(http.StatusUnauthorized)
		}
	})

	t.Run("accepts_the_credential_in_a_header", func(t *testing.T) {
		t.Parallel()
		srv := gatedProbeServer(t, maniflex.ProbesConfig{
			Middleware: []maniflex.HTTPMiddleware{probeToken("s3cret")},
		})
		auth := map[string]string{"X-Probe-Token": "s3cret"}
		for _, path := range probePaths {
			srv.GET(path, auth).AssertStatus(http.StatusOK)
		}
	})

	t.Run("accepts_the_credential_in_the_query_string", func(t *testing.T) {
		// A kubelet httpGet carries a query string far more easily than a
		// rotating header, so this is the form that makes gating /live viable.
		t.Parallel()
		srv := gatedProbeServer(t, maniflex.ProbesConfig{
			Middleware: []maniflex.HTTPMiddleware{probeToken("s3cret")},
		})
		for _, path := range probePaths {
			srv.GET(path + "?token=s3cret").AssertStatus(http.StatusOK)
		}
	})

	t.Run("does_not_leak_onto_any_other_route", func(t *testing.T) {
		// The chain is scoped to the three probe handlers. A model route, the
		// generated documentation, and an unrouted path must not see it.
		t.Parallel()
		var calls int
		var mu sync.Mutex
		srv := gatedProbeServer(t, maniflex.ProbesConfig{
			Middleware: []maniflex.HTTPMiddleware{
				func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						calls++
						mu.Unlock()
						next.ServeHTTP(w, r)
					})
				},
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/openapi.json").AssertStatus(http.StatusOK)
		srv.GET("/nothing-here").AssertStatus(http.StatusNotFound)

		mu.Lock()
		defer mu.Unlock()
		if calls != 0 {
			t.Errorf("probe middleware ran %d time(s) on non-probe routes", calls)
		}
	})
}

// ── Per-probe chains ──────────────────────────────────────────────────────────

func TestProbeGating_PerProbeMiddlewareWrapsOnlyThatProbe(t *testing.T) {
	// The case the shared chain alone cannot express, and the one most
	// deployments want: readiness gated, liveness left open so a rejected
	// request can never cost the pod a SIGKILL mid-drain.
	t.Parallel()
	srv := gatedProbeServer(t, maniflex.ProbesConfig{
		Ready: maniflex.ProbeConfig{
			Middleware: []maniflex.HTTPMiddleware{probeToken("s3cret")},
		},
	})

	srv.GET("/ready").AssertStatus(http.StatusUnauthorized)
	srv.GET("/ready", map[string]string{"X-Probe-Token": "s3cret"}).AssertStatus(http.StatusOK)
	srv.GET("/live").AssertStatus(http.StatusOK)
	srv.GET("/health").AssertStatus(http.StatusOK)
}

func TestProbeGating_SharedChainRunsBeforeTheProbesOwn(t *testing.T) {
	// Append, not override: a probe that declares its own chain still runs the
	// shared one, and runs it first.
	t.Parallel()
	var order []string
	var mu sync.Mutex
	record := func(name string) maniflex.HTTPMiddleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				next.ServeHTTP(w, r)
			})
		}
	}

	srv := gatedProbeServer(t, maniflex.ProbesConfig{
		Middleware: []maniflex.HTTPMiddleware{record("shared-1"), record("shared-2")},
		Ready:      maniflex.ProbeConfig{Middleware: []maniflex.HTTPMiddleware{record("ready-own")}},
	})
	srv.GET("/ready").AssertStatus(http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	want := "shared-1,shared-2,ready-own"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("middleware order: got %q, want %q", got, want)
	}
}

// ── Unmounting ────────────────────────────────────────────────────────────────

func TestProbeGating_DisabledProbeIsNotMounted(t *testing.T) {
	t.Parallel()

	for _, path := range probePaths {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			t.Parallel()
			srv := gatedProbeServer(t, disableProbe(path))

			resp := srv.GET(path).AssertStatus(http.StatusNotFound)
			// Unmounted, not mounted-and-refusing: the answer comes from the
			// router, so it carries no probe body to read a status out of.
			if strings.Contains(string(resp.Body), `"status"`) {
				t.Errorf("disabled %s answered with a probe body: %s", path, resp.Body)
			}
		})
	}
}

func TestProbeGating_DisablingOneProbeLeavesTheOthers(t *testing.T) {
	// Retiring the legacy /health is the expected use, and it must not take
	// liveness or readiness with it.
	t.Parallel()
	srv := gatedProbeServer(t, maniflex.ProbesConfig{
		Health: maniflex.ProbeConfig{Disabled: true},
	})

	srv.GET("/health").AssertStatus(http.StatusNotFound)
	srv.GET("/live").AssertStatus(http.StatusOK)
	srv.GET("/ready").AssertStatus(http.StatusOK)
}

func TestProbeGating_DisabledProbeRunsNoMiddleware(t *testing.T) {
	// Nothing is mounted, so nothing wraps it — a chain declared alongside
	// Disabled is dead configuration, not a handler that answers 404.
	t.Parallel()
	var calls int
	var mu sync.Mutex
	count := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}

	srv := gatedProbeServer(t, maniflex.ProbesConfig{
		Middleware: []maniflex.HTTPMiddleware{count},
		Health: maniflex.ProbeConfig{
			Disabled:   true,
			Middleware: []maniflex.HTTPMiddleware{count},
		},
	})
	srv.GET("/health").AssertStatus(http.StatusNotFound)

	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Errorf("middleware ran %d time(s) for an unmounted probe", calls)
	}
}

func TestProbeGating_AllThreeDisabledMountsNothing(t *testing.T) {
	t.Parallel()
	srv := gatedProbeServer(t, maniflex.ProbesConfig{
		Live:   maniflex.ProbeConfig{Disabled: true},
		Ready:  maniflex.ProbeConfig{Disabled: true},
		Health: maniflex.ProbeConfig{Disabled: true},
	})
	for _, path := range probePaths {
		srv.GET(path).AssertStatus(http.StatusNotFound)
	}
	// The rest of the API is untouched.
	srv.GET("/users").AssertStatus(http.StatusOK)
}

// ── Configuration errors ──────────────────────────────────────────────────────

func TestProbeGating_NilMiddlewarePanics(t *testing.T) {
	// Same contract as Config.Documentation.Middleware and HTTPMiddlewares: a
	// nil entry is a programming error, caught once at router build rather than
	// as a nil dereference on the first probe request.
	t.Parallel()

	cases := []struct {
		name   string
		probes maniflex.ProbesConfig
		want   string
	}{
		{
			name:   "shared_chain",
			probes: maniflex.ProbesConfig{Middleware: []maniflex.HTTPMiddleware{nil}},
			want:   "Probes.Middleware[0]",
		},
		{
			name:   "live_chain",
			probes: maniflex.ProbesConfig{Live: maniflex.ProbeConfig{Middleware: []maniflex.HTTPMiddleware{nil}}},
			want:   "Probes.Live.Middleware[0]",
		},
		{
			name:   "ready_chain",
			probes: maniflex.ProbesConfig{Ready: maniflex.ProbeConfig{Middleware: []maniflex.HTTPMiddleware{nil}}},
			want:   "Probes.Ready.Middleware[0]",
		},
		{
			name:   "health_chain",
			probes: maniflex.ProbesConfig{Health: maniflex.ProbeConfig{Middleware: []maniflex.HTTPMiddleware{nil}}},
			want:   "Probes.Health.Middleware[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := maniflex.New(maniflex.Config{
				PathPrefix:         "/api",
				DisableAutoMigrate: true,
				Probes:             tc.probes,
			})
			server.MustRegister(testutil.DefaultModels()...)

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("a nil %s must panic at router build", tc.want)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, tc.want) {
					t.Errorf("panic message %q must name %s", msg, tc.want)
				}
			}()
			_ = server.Handler()
		})
	}
}

// ── Test infrastructure ───────────────────────────────────────────────────────

// probePaths are the three endpoints Config.Probes governs, relative to the
// path prefix.
var probePaths = []string{"/live", "/ready", "/health"}

func gatedProbeServer(t *testing.T, probes maniflex.ProbesConfig) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) { cfg.Probes = probes },
	})
}

// disableProbe builds the ProbesConfig that unmounts exactly one probe.
func disableProbe(path string) maniflex.ProbesConfig {
	off := maniflex.ProbeConfig{Disabled: true}
	switch path {
	case "/live":
		return maniflex.ProbesConfig{Live: off}
	case "/ready":
		return maniflex.ProbesConfig{Ready: off}
	case "/health":
		return maniflex.ProbesConfig{Health: off}
	}
	panic(fmt.Sprintf("unknown probe path %q", path))
}

// probeToken is the shape of gate this feature exists for: a shared secret read
// from either a header or the query string, so the same middleware works for a
// human with curl and for an orchestrator that can only vary the probe URL.
func probeToken(want string) maniflex.HTTPMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Probe-Token")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if got != want {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
