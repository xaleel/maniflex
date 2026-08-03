package e2e

// probes_test.go tests the split liveness/readiness endpoints (GAP-01):
//
//	GET {prefix}/live   — process liveness. Never touches a dependency and stays
//	                      200 while the process is up, shutdown included.
//	GET {prefix}/ready  — readiness. Lifecycle state first, then the database and
//	                      every Config.ReadinessChecks entry, all bounded by one
//	                      HealthTimeout budget.
//
// GET {prefix}/health keeps its previous behaviour and is covered by
// health_test.go.
//
// Run this group:
//
//	go test ./tests/e2e/... -run 'TestLivenessProbe|TestReadinessProbe|TestReadinessLifecycle|TestReadinessCheckValidation'

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Liveness ──────────────────────────────────────────────────────────────────

func TestLivenessProbe(t *testing.T) {
	t.Parallel()

	t.Run("returns_200_ok", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/live").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
		})
	})

	t.Run("stays_200_when_the_database_is_unreachable", func(t *testing.T) {
		// The whole point of the split: a dependency blip must not make
		// Kubernetes restart a healthy process.
		t.Parallel()
		srv := newProbeServer(t, probeOptions{pingErr: context.DeadlineExceeded})
		srv.GET("/live").AssertStatus(http.StatusOK)
	})

	t.Run("reports_no_dependency_checks", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/live").AssertJSON(func(body map[string]any) {
			if _, has := body["checks"]; has {
				t.Errorf("liveness must not report dependency checks, got %v", body)
			}
		})
	})

	t.Run("stays_200_after_shutdown", func(t *testing.T) {
		// Liveness answering 503 mid-drain would earn the pod a SIGKILL before
		// its in-flight requests finished.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		srv.GET("/live").AssertStatus(http.StatusOK)
	})

	t.Run("bypasses_pipeline_auth", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "no access")
					return nil
				})
			},
		})
		srv.GET("/live").AssertStatus(http.StatusOK)
	})

	t.Run("is_not_cached", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/live")
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control: got %q, want %q", got, "no-store")
		}
	})
}

// ── Readiness: dependencies ───────────────────────────────────────────────────

func TestReadinessProbe(t *testing.T) {
	t.Parallel()

	t.Run("pings_the_database_without_HealthCheckDB", func(t *testing.T) {
		// HealthCheckDB governs the legacy /health endpoint only. Readiness that
		// does not check its dependencies is not readiness.
		t.Parallel()
		srv := newProbeServer(t, probeOptions{realSQLite: true})
		srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "ok")
		})
	})

	t.Run("returns_503_when_the_database_ping_fails", func(t *testing.T) {
		t.Parallel()
		srv := newProbeServer(t, probeOptions{pingErr: context.DeadlineExceeded})
		srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
			AssertJSON(func(body map[string]any) {
				testutil.AssertEqual(t, "status", body["status"], "not_ready")
				testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "error")
			})
	})

	t.Run("omits_the_raw_driver_error", func(t *testing.T) {
		// Same rule as /health: driver text can carry DSN fragments, so it is
		// logged and never echoed.
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			pingErr: errors.New("dial tcp 10.0.0.7:5432: connect: connection refused"),
		})
		resp := srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
		if strings.Contains(string(resp.Body), "10.0.0.7") {
			t.Errorf("readiness body leaked the driver error: %s", resp.Body)
		}
	})

	t.Run("adapter_without_ping_reports_unknown_and_stays_ready", func(t *testing.T) {
		t.Parallel()
		srv := newProbeServer(t, probeOptions{noPinger: true})
		srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "unknown")
		})
	})

	t.Run("passing_custom_check_is_reported_by_name", func(t *testing.T) {
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			realSQLite: true,
			checks: []maniflex.ReadinessCheck{{
				Name:  "cache",
				Check: func(context.Context) error { return nil },
			}},
		})
		srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "cache check", checkValue(t, body, "cache"), "ok")
		})
	})

	t.Run("failing_custom_check_returns_503", func(t *testing.T) {
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			realSQLite: true,
			checks: []maniflex.ReadinessCheck{{
				Name:  "broker",
				Check: func(context.Context) error { return errors.New("no connection") },
			}},
		})
		srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
			AssertJSON(func(body map[string]any) {
				testutil.AssertEqual(t, "status", body["status"], "not_ready")
				testutil.AssertEqual(t, "broker check", checkValue(t, body, "broker"), "error")
				// A healthy dependency is still reported as healthy.
				testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "ok")
			})
	})

	t.Run("custom_check_error_text_is_not_echoed", func(t *testing.T) {
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			realSQLite: true,
			checks: []maniflex.ReadinessCheck{{
				Name:  "broker",
				Check: func(context.Context) error { return errors.New("amqp://user:hunter2@broker") },
			}},
		})
		resp := srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
		if strings.Contains(string(resp.Body), "hunter2") {
			t.Errorf("readiness body leaked a check error: %s", resp.Body)
		}
	})

	t.Run("panicking_custom_check_is_a_failure_not_a_crash", func(t *testing.T) {
		// Checks run on their own goroutines, where the router's PanicRecoverer
		// cannot reach them — an unrecovered panic would take the process down.
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			realSQLite: true,
			checks: []maniflex.ReadinessCheck{{
				Name:  "broker",
				Check: func(context.Context) error { panic("boom") },
			}},
		})
		srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
			AssertJSON(func(body map[string]any) {
				testutil.AssertEqual(t, "broker check", checkValue(t, body, "broker"), "error")
			})
	})

	t.Run("hanging_check_is_bounded_by_HealthTimeout", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		srv := newProbeServer(t, probeOptions{
			realSQLite:    true,
			healthTimeout: 50 * time.Millisecond,
			checks: []maniflex.ReadinessCheck{{
				Name: "broker",
				Check: func(ctx context.Context) error {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
						return nil
					}
				},
			}},
		})

		start := time.Now()
		srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("readiness took %s, must return within ~50ms", elapsed)
		}
	})

	t.Run("bypasses_pipeline_auth", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "no access")
					return nil
				})
			},
		})
		srv.GET("/ready").AssertStatus(http.StatusOK)
	})

	t.Run("is_not_cached", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/ready")
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("Cache-Control: got %q, want %q", got, "no-store")
		}
	})
}

// ── Readiness: lifecycle transitions ──────────────────────────────────────────

func TestReadinessLifecycle(t *testing.T) {
	t.Parallel()

	t.Run("embedded_handler_that_never_starts_is_ready", func(t *testing.T) {
		// A Handler-only embedding never leaves the "new" state. Gating on the
		// lifecycle there would leave it permanently not-ready.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
		})
	})

	t.Run("reports_starting_while_a_service_is_still_coming_up", func(t *testing.T) {
		t.Parallel()
		blocked := make(chan struct{})
		release := make(chan struct{})
		server, ts := newEmbeddedProbeServer(t, &blockingService{blocked: blocked, release: release})

		started := make(chan error, 1)
		go func() { started <- server.StartServices() }()

		<-blocked // the service is in Start; the lifecycle is "starting"
		resp := probeGET(t, ts, "/api/ready")
		if resp.status != http.StatusServiceUnavailable {
			t.Fatalf("readiness during startup: got %d, want 503", resp.status)
		}
		testutil.AssertEqual(t, "status", resp.body["status"], "starting")
		// A service still starting says nothing about the process being alive.
		if live := probeGET(t, ts, "/api/live"); live.status != http.StatusOK {
			t.Errorf("liveness during startup: got %d, want 200", live.status)
		}

		close(release)
		if err := <-started; err != nil {
			t.Fatalf("StartServices: %v", err)
		}
		if resp := probeGET(t, ts, "/api/ready"); resp.status != http.StatusOK {
			t.Errorf("readiness once services are running: got %d, want 200", resp.status)
		}
	})

	t.Run("reports_stopping_once_shutdown_begins", func(t *testing.T) {
		t.Parallel()
		server, ts := newEmbeddedProbeServer(t, nil)

		if resp := probeGET(t, ts, "/api/ready"); resp.status != http.StatusOK {
			t.Fatalf("readiness before shutdown: got %d, want 200", resp.status)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}

		resp := probeGET(t, ts, "/api/ready")
		if resp.status != http.StatusServiceUnavailable {
			t.Fatalf("readiness after shutdown: got %d, want 503", resp.status)
		}
		testutil.AssertEqual(t, "status", resp.body["status"], "stopping")
	})

	t.Run("stopping_is_reported_without_touching_the_database", func(t *testing.T) {
		// Draining must deregister the pod promptly; waiting on a dependency
		// first would delay it by up to HealthTimeout.
		t.Parallel()
		srv := newProbeServer(t, probeOptions{
			healthTimeout: 30 * time.Second,
			checks: []maniflex.ReadinessCheck{{
				Name:  "broker",
				Check: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			}},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}

		start := time.Now()
		srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
			AssertJSON(func(body map[string]any) {
				testutil.AssertEqual(t, "status", body["status"], "stopping")
				if _, has := body["checks"]; has {
					t.Errorf("stopping readiness must not run checks, got %v", body)
				}
			})
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("stopping readiness waited %s on a dependency check", elapsed)
		}
	})
}

// ── Readiness check validation ────────────────────────────────────────────────

func TestReadinessCheckValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		checks []maniflex.ReadinessCheck
		want   string
	}{
		{
			name:   "nil_check_func",
			checks: []maniflex.ReadinessCheck{{Name: "cache"}},
			want:   "Check",
		},
		{
			name:   "empty_name",
			checks: []maniflex.ReadinessCheck{{Check: func(context.Context) error { return nil }}},
			want:   "Name",
		},
		{
			name: "duplicate_name",
			checks: []maniflex.ReadinessCheck{
				{Name: "cache", Check: func(context.Context) error { return nil }},
				{Name: "cache", Check: func(context.Context) error { return nil }},
			},
			want: "duplicate",
		},
		{
			name: "reserved_db_name",
			checks: []maniflex.ReadinessCheck{
				{Name: "db", Check: func(context.Context) error { return nil }},
			},
			want: "reserved",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := maniflex.New(maniflex.Config{
				PathPrefix:         "/api",
				DisableAutoMigrate: true,
				ReadinessChecks:    tc.checks,
			})
			server.MustRegister(testutil.DefaultModels()...)

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s must panic at router build", tc.name)
				}
				msg, _ := r.(string)
				if !strings.Contains(msg, "ReadinessChecks") || !strings.Contains(msg, tc.want) {
					t.Errorf("panic message %q must name ReadinessChecks and %q", msg, tc.want)
				}
			}()
			_ = server.Handler()
		})
	}
}

// ── Test infrastructure ───────────────────────────────────────────────────────

type probeOptions struct {
	pingErr       error // non-nil: the adapter's Ping returns this error
	noPinger      bool  // adapter does not implement Pinger
	realSQLite    bool  // use the real sqlite adapter (satisfies Pinger)
	healthTimeout time.Duration
	checks        []maniflex.ReadinessCheck
}

// newProbeServer builds a test server whose readiness dependencies are
// controlled by probeOptions. HealthCheckDB stays off throughout: readiness
// does not depend on it.
//
// PublishReadinessChecks is on because these tests assert on per-dependency
// results, which are opt-in since DOC-6. The default — that the map is absent —
// is covered by probes_hardening_test.go.
func newProbeServer(t *testing.T, opts probeOptions) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Probes.PublishReadinessChecks = true
			cfg.ReadinessChecks = opts.checks
			cfg.HealthTimeout = opts.healthTimeout
		},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			real, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			switch {
			case opts.realSQLite:
				return real, nil
			case opts.noPinger:
				return &noPingAdapter{DBAdapter: real}, nil
			default:
				return &mockPingAdapter{DBAdapter: real, pingErr: opts.pingErr}, nil
			}
		},
	})
}

// newEmbeddedProbeServer returns an embedded-style server — the caller owns the
// listener — so the test can drive StartServices and Shutdown around it and
// watch the probes follow the lifecycle.
func newEmbeddedProbeServer(t *testing.T, svc maniflex.Service) (*maniflex.Server, *httptest.Server) {
	t.Helper()
	server := maniflex.New(maniflex.Config{
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
		HealthTimeout:      time.Second,
	})
	server.MustRegister(testutil.DefaultModels()...)

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)

	if svc != nil {
		server.AddService(svc)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return server, ts
}

type probeResponse struct {
	status int
	body   map[string]any
}

func probeGET(t *testing.T, ts *httptest.Server, path string) probeResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s body: %v", path, err)
	}
	return probeResponse{status: resp.StatusCode, body: body}
}

// checkValue reads one entry out of the readiness "checks" object.
func checkValue(t *testing.T, body map[string]any, name string) any {
	t.Helper()
	checks, ok := body["checks"].(map[string]any)
	if !ok {
		t.Fatalf("readiness body has no checks object: %v", body)
	}
	v, ok := checks[name]
	if !ok {
		t.Fatalf("readiness checks have no %q entry: %v", name, checks)
	}
	return v
}

// blockingService parks in Start until released, holding the lifecycle in its
// starting phase.
type blockingService struct {
	blocked chan struct{}
	release chan struct{}
}

func (s *blockingService) Start(context.Context) error {
	close(s.blocked)
	<-s.release
	return nil
}

func (s *blockingService) Stop(context.Context) error { return nil }

var _ maniflex.Service = (*blockingService)(nil)
