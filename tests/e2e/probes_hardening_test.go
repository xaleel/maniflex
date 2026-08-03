package e2e

// probes_hardening_test.go covers the two things a public {prefix}/ready used
// to give away for free (audit DOC-6):
//
//	the dependency map  — the body named every ReadinessChecks entry and said
//	                      which were failing. The names are now opt-in via
//	                      Probes.PublishReadinessChecks; the status code, which
//	                      is all an orchestrator reads, is unchanged.
//	the fan-out         — every request pinged the database and ran every check.
//	                      Concurrent requests now share one run.
//
// Run this group:
//
//	go test ./tests/e2e/... -run 'TestReadinessDetail|TestProbeCoalescing'

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── The dependency map ────────────────────────────────────────────────────────

func TestReadinessDetail_HiddenByDefault(t *testing.T) {
	t.Parallel()

	t.Run("healthy", func(t *testing.T) {
		t.Parallel()
		srv := detailServer(t, false, passingCheck("billing"))
		srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			assertNoChecks(t, body)
		})
	})

	t.Run("degraded", func(t *testing.T) {
		// The status code is the orchestrator's whole contract, so withholding
		// the names costs it nothing.
		t.Parallel()
		srv := detailServer(t, false, failingCheck("billing"))
		resp := srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
		resp.AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "not_ready")
			assertNoChecks(t, body)
		})
		// Not merely absent from the checks object — absent from the response.
		if strings.Contains(string(resp.Body), "billing") {
			t.Errorf("readiness body named a dependency: %s", resp.Body)
		}
	})
}

func TestReadinessDetail_PublishedWhenOptedIn(t *testing.T) {
	t.Parallel()
	srv := detailServer(t, true, failingCheck("billing"))
	srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
		AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "not_ready")
			testutil.AssertEqual(t, "billing check", checkValue(t, body, "billing"), "error")
			testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "ok")
		})
}

func TestReadinessDetail_FailingCheckIsStillLogged(t *testing.T) {
	// Hiding the map must not make a 503 undiagnosable: the name goes to the
	// operator's log either way, which is where it was already going.
	t.Parallel()
	var logs bytes.Buffer
	srv := testutil.NewServer(t, testutil.Options{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})),
		Config: func(cfg *maniflex.Config) {
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{failingCheck("billing")}
		},
	})

	resp := srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
	if strings.Contains(string(resp.Body), "billing") {
		t.Errorf("readiness body named a dependency: %s", resp.Body)
	}
	if !strings.Contains(logs.String(), "billing") {
		t.Errorf("the failing check must be logged, got: %s", logs.String())
	}
}

func TestReadinessDetail_HealthKeepsItsDbKey(t *testing.T) {
	// /health reports one fixed key the framework owns, not a name the
	// application chose, so it says nothing about the app's topology and is
	// left alone.
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{HealthCheckDB: true})
	srv.GET("/health").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		testutil.AssertEqual(t, "status", body["status"], "ok")
		testutil.AssertEqual(t, "db", body["db"], "ok")
	})
}

// ── The fan-out ───────────────────────────────────────────────────────────────

const coalesceRequests = 20

func TestProbeCoalescing_ConcurrentReadinessSharesOneRun(t *testing.T) {
	t.Parallel()

	var runs atomic.Int64
	gate := newArrivalGate(coalesceRequests)

	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			// Hold every request at the door until all of them have arrived, so
			// the test does not depend on how fast they are dispatched.
			cfg.Probes.Ready.Middleware = []maniflex.HTTPMiddleware{gate.middleware}
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name: "billing",
				Check: func(context.Context) error {
					runs.Add(1)
					// Keep the flight open long enough for every released
					// request to reach it and join.
					gate.hold()
					return nil
				},
			}}
		},
	})

	statuses := make([]int, coalesceRequests)
	var wg sync.WaitGroup
	for i := range coalesceRequests {
		wg.Go(func() { statuses[i] = srv.GET("/ready").Status })
	}
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Errorf("%d concurrent probes ran the check %d time(s), want 1", coalesceRequests, got)
	}
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Errorf("request %d: got %d, want 200", i, status)
		}
	}
}

func TestProbeCoalescing_LaterRequestGetsAFreshRun(t *testing.T) {
	// Coalescing, not caching: joining an open flight is free, but a request
	// that arrives after it closed must see the dependency as it is now. A
	// cached "ok" would keep a pod in the load balancer after its database
	// went away.
	t.Parallel()

	var runs atomic.Int64
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name:  "billing",
				Check: func(context.Context) error { runs.Add(1); return nil },
			}}
		},
	})

	for i := 1; i <= 3; i++ {
		srv.GET("/ready").AssertStatus(http.StatusOK)
		if got := runs.Load(); got != int64(i) {
			t.Fatalf("after %d sequential probes the check ran %d time(s), want %d", i, got, i)
		}
	}
}

func TestProbeCoalescing_ChangedDependencyIsSeenByTheNextRequest(t *testing.T) {
	// The same rule stated as an outcome rather than a call count.
	t.Parallel()

	var failing atomic.Bool
	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name: "billing",
				Check: func(context.Context) error {
					if failing.Load() {
						return errors.New("down")
					}
					return nil
				},
			}}
		},
	})

	srv.GET("/ready").AssertStatus(http.StatusOK)
	failing.Store(true)
	srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)
	failing.Store(false)
	srv.GET("/ready").AssertStatus(http.StatusOK)
}

func TestProbeCoalescing_ConcurrentHealthSharesOnePing(t *testing.T) {
	// /health pings the database too when HealthCheckDB is on, so it carries
	// the same amplification and gets the same treatment.
	t.Parallel()

	gate := newArrivalGate(coalesceRequests)
	adapter := &countingPingAdapter{onPing: gate.hold}

	srv := testutil.NewServer(t, testutil.Options{
		HealthCheckDB: true,
		Config: func(cfg *maniflex.Config) {
			cfg.Probes.Health.Middleware = []maniflex.HTTPMiddleware{gate.middleware}
		},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			real, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			adapter.DBAdapter = real
			return adapter, nil
		},
	})

	var wg sync.WaitGroup
	for range coalesceRequests {
		wg.Go(func() { srv.GET("/health").AssertStatus(http.StatusOK) })
	}
	wg.Wait()

	if got := adapter.pings.Load(); got != 1 {
		t.Errorf("%d concurrent /health probes pinged %d time(s), want 1", coalesceRequests, got)
	}
}

func TestProbeCoalescing_ReadyAndHealthDoNotShareAFlight(t *testing.T) {
	// Two endpoints, two answers. /ready must never be served a result that
	// /health computed under different rules — HealthCheckDB governs one and
	// not the other.
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		// HealthCheckDB off: /health does no I/O and reports no db key.
		Config: func(cfg *maniflex.Config) { cfg.Probes.PublishReadinessChecks = true },
	})

	srv.GET("/health").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		if _, has := body["db"]; has {
			t.Errorf("/health must not check the database with HealthCheckDB off: %v", body)
		}
	})
	srv.GET("/ready").AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		testutil.AssertEqual(t, "db check", checkValue(t, body, "db"), "ok")
	})
}

// ── A panicking adapter ───────────────────────────────────────────────────────

func TestProbePanickingPing_IsAFailedCheckNotACrash(t *testing.T) {
	// Ping is third-party code, and on the readiness path it runs on its own
	// goroutine where the router's PanicRecoverer cannot reach it — so an
	// adapter that panicked took the whole process down from an
	// unauthenticated probe request. If this regresses the test binary dies
	// rather than reporting a failure, which is the point.
	t.Parallel()

	srv := panicPingServer(t, false)

	srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable).
		AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "not_ready")
		})
}

func TestProbePanickingPing_HealthReportsDegraded(t *testing.T) {
	// /health runs the ping on the request goroutine, so it was already
	// recovered — as a 500 PANIC. A dependency that misbehaves is a degraded
	// dependency, which is the answer the endpoint exists to give.
	t.Parallel()

	srv := panicPingServer(t, true)

	srv.GET("/health").AssertStatus(http.StatusServiceUnavailable).
		AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "degraded")
			testutil.AssertEqual(t, "db", body["db"], "error")
		})
}

func TestProbePanickingPing_IsLogged(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	srv := testutil.NewServer(t, testutil.Options{
		Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})),
		DBAdapter: panicPingAdapterFor(t),
	})
	srv.GET("/ready").AssertStatus(http.StatusServiceUnavailable)

	if !strings.Contains(logs.String(), "panicked") {
		t.Errorf("a panicking ping must be logged, got: %s", logs.String())
	}
}

// ── Test infrastructure ───────────────────────────────────────────────────────

func detailServer(t *testing.T, publish bool, checks ...maniflex.ReadinessCheck) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Probes.PublishReadinessChecks = publish
			cfg.ReadinessChecks = checks
		},
	})
}

func passingCheck(name string) maniflex.ReadinessCheck {
	return maniflex.ReadinessCheck{Name: name, Check: func(context.Context) error { return nil }}
}

func failingCheck(name string) maniflex.ReadinessCheck {
	return maniflex.ReadinessCheck{
		Name:  name,
		Check: func(context.Context) error { return errors.New("dependency is down") },
	}
}

func assertNoChecks(t *testing.T, body map[string]any) {
	t.Helper()
	if _, has := body["checks"]; has {
		t.Errorf("readiness published its dependency map by default: %v", body)
	}
}

// arrivalGate makes a coalescing test independent of dispatch timing. Its
// middleware parks each request until `want` of them have arrived, so they
// enter the handler together; hold then keeps the first flight open until every
// one of them has had the chance to join it.
type arrivalGate struct {
	want     int
	arrived  atomic.Int64
	released chan struct{}
	once     sync.Once
}

func newArrivalGate(want int) *arrivalGate {
	return &arrivalGate{want: want, released: make(chan struct{})}
}

func (g *arrivalGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(g.arrived.Add(1)) >= g.want {
			g.once.Do(func() { close(g.released) })
		}
		<-g.released
		next.ServeHTTP(w, r)
	})
}

// hold blocks inside the dependency check, keeping the first flight open.
//
// Every request is already past the gate when this runs — that is what the
// gate is for — so each is only a few instructions from the flight it will
// join. The pause covers that gap. If it were ever too short the test would
// report more runs than 1, which is a visible failure rather than a silent
// weakening of the assertion.
func (g *arrivalGate) hold() {
	<-g.released
	time.Sleep(150 * time.Millisecond)
}

func panicPingServer(t *testing.T, healthCheckDB bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		HealthCheckDB: healthCheckDB,
		DBAdapter:     panicPingAdapterFor(t),
	})
}

func panicPingAdapterFor(t *testing.T) func(maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
	t.Helper()
	return func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
		real, err := sqlite.Open(":memory:", reg)
		if err != nil {
			return nil, err
		}
		return &panicPingAdapter{DBAdapter: real}, nil
	}
}

// panicPingAdapter stands in for any third-party adapter whose Ping misbehaves.
type panicPingAdapter struct{ maniflex.DBAdapter }

func (*panicPingAdapter) Ping(context.Context) error { panic("adapter ping exploded") }

var _ maniflex.DBAdapter = (*panicPingAdapter)(nil)

// countingPingAdapter records how many times the database was actually pinged.
type countingPingAdapter struct {
	maniflex.DBAdapter
	pings  atomic.Int64
	onPing func()
}

func (a *countingPingAdapter) Ping(context.Context) error {
	a.pings.Add(1)
	if a.onPing != nil {
		a.onPing()
	}
	return nil
}

var _ maniflex.DBAdapter = (*countingPingAdapter)(nil)
