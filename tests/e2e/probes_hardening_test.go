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

// ── A flight outlives the request that opened it ──────────────────────────────
//
// The readiness flight is shared, so binding it to one participant's lifetime
// makes that participant's disconnect everyone's failure (audit M3). A kubelet
// probe that times out and drops its connection cancels the request context,
// and every request coalesced onto that flight is handed the resulting
// failure — a healthy server answering 503 and, since 503 on readiness pulls
// the pod out of the load balancer's endpoints, taking itself out of service.
//
// The cause is tested directly rather than through its consequence: the
// context the checks run on must not die with the request that opened the
// flight. Values are still inherited, so anything request-scoped a check reads
// keeps working; only cancellation and the deadline are severed, and
// Config.HealthTimeout still bounds the run.

func TestProbeFlight_CheckOutlivesTheRequestThatOpenedIt(t *testing.T) {
	t.Parallel()

	// Long enough that a cancellation which is going to propagate has done so.
	// The asymmetry is deliberate: with the bug the context dies in
	// milliseconds and the test fails immediately, so this is only ever paid
	// when passing.
	const grace = 1500 * time.Millisecond

	var startOnce sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	ctxCh := make(chan context.Context, 1)

	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name: "billing",
				Check: func(ctx context.Context) error {
					startOnce.Do(func() {
						ctxCh <- ctx
						close(started)
					})
					<-release
					return nil
				},
			}}
		},
	})

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.APIPath("/ready"), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := srv.Client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	<-started // the flight is open and the checks are running
	checkCtx := <-ctxCh
	cancel() // the orchestrator gives up on its probe and drops the connection
	<-done   // and the client side is definitively gone

	select {
	case <-checkCtx.Done():
		t.Errorf("the readiness checks' context was cancelled when the request that opened "+
			"the flight went away (%v); every request coalesced onto this flight is handed "+
			"that failure, so one disconnecting probe answers 503 for all of them from a "+
			"healthy server", context.Cause(checkCtx))
	case <-time.After(grace):
		// Survived the initiator, which is the point.
	}

	close(release)
}

func TestProbeFlight_WaiterSurvivesTheInitiatorDisconnecting(t *testing.T) {
	// The consequence of the above, end to end: a probe that joins an open
	// flight must not inherit the 503 caused by the initiator hanging up.
	t.Parallel()

	var runs, arrived atomic.Int64
	var startOnce, secondOnce sync.Once
	started := make(chan struct{})
	secondArrived := make(chan struct{})
	release := make(chan struct{})

	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.Probes.Ready.Middleware = []maniflex.HTTPMiddleware{
				func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if arrived.Add(1) >= 2 {
							secondOnce.Do(func() { close(secondArrived) })
						}
						next.ServeHTTP(w, r)
					})
				},
			}
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name: "billing",
				Check: func(ctx context.Context) error {
					runs.Add(1)
					startOnce.Do(func() { close(started) })
					<-release
					// A check that respects its context fails exactly this way
					// when the context it was handed has been cancelled.
					return ctx.Err()
				},
			}}
		},
	})

	// The initiator, which will hang up mid-flight.
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.APIPath("/ready"), nil)
	if err != nil {
		t.Fatal(err)
	}
	initiatorDone := make(chan struct{})
	go func() {
		defer close(initiatorDone)
		if resp, err := srv.Client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	<-started
	cancel()
	<-initiatorDone

	// The waiter, which joins the flight the initiator left behind.
	waiterStatus := make(chan int, 1)
	go func() { waiterStatus <- srv.GET("/ready").Status }()

	<-secondArrived
	// The waiter is past the door and a few instructions from the flight it
	// will join. As with arrivalGate.hold, too short a pause shows up as a
	// second run below — a visible failure, not a quiet one.
	time.Sleep(150 * time.Millisecond)
	close(release)

	status := <-waiterStatus
	if got := runs.Load(); got != 1 {
		t.Fatalf("precondition: the check ran %d time(s), want 1 — the waiter did not join "+
			"the initiator's flight, so this run proves nothing about coalesced failures", got)
	}
	if status != http.StatusOK {
		t.Errorf("a probe that joined an open flight answered %d; the server is healthy and "+
			"only the request that opened the flight went away", status)
	}
}

func TestProbeFlight_HealthTimeoutStillBoundsTheFlight(t *testing.T) {
	// Severing the initiator's cancellation leaves HealthTimeout as the only
	// thing keeping the flight finite, and nothing covered that for /ready --
	// health_test.go bounds /health alone. Without this a regression here would
	// not fail, it would hang: a check that never returns holds the flight open
	// and every later probe blocks on it forever.
	t.Parallel()
	const budget = 100 * time.Millisecond

	srv := testutil.NewServer(t, testutil.Options{
		Config: func(cfg *maniflex.Config) {
			cfg.HealthTimeout = budget
			cfg.ReadinessChecks = []maniflex.ReadinessCheck{{
				Name: "billing",
				Check: func(ctx context.Context) error {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(10 * time.Second):
						// Returns rather than blocking forever, so a broken
						// budget is a failed assertion and not a hung suite.
						return nil
					}
				},
			}}
		},
	})

	start := time.Now()
	status := srv.GET("/ready").Status
	elapsed := time.Since(start)

	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a dependency that outran the %v budget is not ready",
			status, budget)
	}
	if elapsed > 20*budget {
		t.Errorf("readiness answered after %v with a %v HealthTimeout — the budget no longer "+
			"bounds the flight, which is now the only thing that does", elapsed, budget)
	}
}
