package maniflex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// ReadinessCheck is one named dependency probe reported by GET {prefix}/ready.
//
// The framework contributes the database check itself under the reserved name
// "db"; everything else an application depends on — a cache, a broker, an
// upstream API — is declared through Config.ReadinessChecks:
//
//	Config{ReadinessChecks: []maniflex.ReadinessCheck{{
//	    Name:  "broker",
//	    Check: func(ctx context.Context) error { return broker.Ping(ctx) },
//	}}}
//
// Check runs on every readiness request, so keep it cheap — a connection-pool
// probe, not a full round-trip through the dependency. All checks run
// concurrently and share one Config.HealthTimeout budget; honour the ctx.
//
// A non-nil error (or a panic, which is recovered) makes the endpoint answer
// 503. The error is logged through Config.Logger and never written to the
// response: a probe body is the one place an unauthenticated client reads
// straight from a dependency, and connection strings live in those messages.
// Name is withheld too unless Config.Probes.PublishReadinessChecks is set — it
// says what this application is built on, and which part of it is failing.
type ReadinessCheck struct {
	// Name identifies the check in the response body. It must be non-empty,
	// unique, and not "db", which the framework reserves for its own check.
	Name string

	// Check reports whether the dependency is usable. It must not be nil.
	Check func(ctx context.Context) error
}

// readinessDBCheck is the name the framework's own database check reports under.
const readinessDBCheck = "db"

// Check outcomes as they appear in the readiness body. "unknown" is not a
// failure: it says the framework has no way to test the dependency (an adapter
// that does not implement Pinger, or no adapter configured at all), which is a
// deployment fact rather than an outage.
const (
	probeOK      = "ok"
	probeError   = "error"
	probeUnknown = "unknown"
)

// mountProbes registers the three probe endpoints under the path prefix,
// subject to Config.Probes (audit DOC-4).
//
// The probes never enter the model pipeline, so Pipeline.Auth does not run for
// them and there is nothing for authx.AllowPublic to exempt them from. That is
// the intended posture — an orchestrator's probe is the canonical
// unauthenticated request — and Config.Probes is the override, the only lever
// scoped to the probes alone. (Config.HTTPMiddlewares reaches them too, but it
// reaches every other route at the same time.)
//
// A disabled probe is not mounted at all, so the router answers 404 and neither
// middleware chain runs. Mounting a handler that refused would be a worse
// answer to the same question: it says the endpoint exists.
func mountProbes(r chi.Router, cfg *Config, reg *Registry, phase func() readinessPhase) {
	probes := cfg.Probes

	// Validated up front, and for every probe rather than only the mounted
	// ones: a nil entry is a typo either way, and finding it the day someone
	// re-enables the probe is finding it too late.
	validateProbeMiddleware(probes.Middleware, "Probes.Middleware")
	validateProbeMiddleware(probes.Live.Middleware, "Probes.Live.Middleware")
	validateProbeMiddleware(probes.Ready.Middleware, "Probes.Ready.Middleware")
	validateProbeMiddleware(probes.Health.Middleware, "Probes.Health.Middleware")

	mount := func(probe ProbeConfig, path string, h http.HandlerFunc) {
		if probe.Disabled {
			return
		}
		// The shared chain first, then the probe's own — appended, not
		// replaced, so declaring one probe's chain cannot silently drop the
		// policy the operator meant to apply to all of them.
		router := r
		for _, mw := range probes.Middleware {
			router = router.With(mw)
		}
		for _, mw := range probe.Middleware {
			router = router.With(mw)
		}
		router.Get(path, h)
	}

	mount(probes.Live, "/live", liveHandler())
	mount(probes.Ready, "/ready", readyHandler(cfg, reg, phase))
	mount(probes.Health, "/health", healthHandler(cfg, reg))
}

// validateProbeMiddleware panics on a nil entry, naming the field so the
// message points at the configuration rather than at a nil dereference inside
// chi. Same contract as Config.HTTPMiddlewares and Documentation.Middleware.
func validateProbeMiddleware(chain []HTTPMiddleware, field string) {
	for i, mw := range chain {
		if mw == nil {
			panic(fmt.Sprintf("maniflex: Config.%s[%d] must not be nil", field, i))
		}
	}
}

// probeFlight collapses concurrent probe requests onto one dependency check
// (audit DOC-6).
//
// A probe endpoint is unauthenticated by design, so without this every request
// that reaches it costs a database ping plus one call per ReadinessCheck, and a
// flood of probes becomes a flood against the dependencies they name. Requests
// arriving while a check is running now wait for it and share its answer.
//
// It coalesces; it does not cache. A request that arrives after the flight
// closed starts a new one, because readiness that is even slightly stale keeps
// a pod in the load balancer after its database has gone away. The saving is in
// concurrency, which is where the amplification is: serial requests are already
// bounded by how long a check takes.
//
// This is x/sync/singleflight in miniature. The core module depends on chi and
// uuid and nothing else, and that is worth more than the twenty lines below.
type probeFlight[T any] struct {
	mu      sync.Mutex
	current *probeFlightCall[T]
}

type probeFlightCall[T any] struct {
	done     chan struct{}
	result   T
	panicked any
	failed   bool
}

// do runs work, or joins the run already in progress and returns its result.
//
// A panic reaches every participant. Handing the waiters a zero value instead
// would be a fail-open: a readiness map that is nil reads as "no check
// failed", so a panicking adapter would answer 200 to everyone who happened to
// be waiting on it. An adapter's Ping is third-party code and nothing else on
// this path recovers it.
func (f *probeFlight[T]) do(work func() T) T {
	f.mu.Lock()
	if call := f.current; call != nil {
		f.mu.Unlock()
		<-call.done
		if call.failed {
			panic(call.panicked)
		}
		return call.result
	}
	call := &probeFlightCall[T]{done: make(chan struct{})}
	f.current = call
	f.mu.Unlock()

	// Released before the waiters are woken, so a request that arrives while
	// they drain opens a fresh flight rather than joining a finished one.
	release := func() {
		f.mu.Lock()
		f.current = nil
		f.mu.Unlock()
		close(call.done)
	}
	defer func() {
		if r := recover(); r != nil {
			call.panicked, call.failed = r, true
			release()
			panic(r) // the initiator keeps its own panic, and its stack
		}
		release()
	}()

	call.result = work()
	return call.result
}

// readinessPhase is the lifecycle summary readiness answers from before it
// looks at any dependency.
type readinessPhase uint8

const (
	// phaseServing covers a running server and one that never starts a
	// lifecycle at all — the Handler-only embedding, where the caller owns the
	// listener and the framework is never told when serving begins. Gating on
	// the lifecycle there would leave such a deployment permanently not-ready.
	phaseServing readinessPhase = iota
	phaseStarting
	phaseStopping
)

// probeBody is the wire shape of both probe responses. Checks are omitted
// whenever none ran, so liveness and the two lifecycle answers stay minimal.
type probeBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func writeProbe(w http.ResponseWriter, code int, body probeBody) {
	w.Header().Set("Content-Type", "application/json")
	// A cached probe answer is a stale probe answer.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// liveHandler returns the handler for GET {prefix}/live: is this process alive?
//
// It performs no I/O and consults no dependency, so it answers 200 for as long
// as the process can serve a request — during startup, during a database
// outage, and throughout the graceful drain. That last case is the reason the
// endpoint exists separately: a liveness probe that fails while the server is
// draining earns the pod a SIGKILL in the middle of its in-flight requests.
//
// Readiness — should this process receive traffic? — is GET {prefix}/ready.
func liveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeProbe(w, http.StatusOK, probeBody{Status: probeOK})
	}
}

// readyHandler returns the handler for GET {prefix}/ready: should this process
// receive traffic?
//
// The lifecycle is consulted first, and answers on its own when the server is
// not serving yet or is on its way down:
//
//	HTTP 503  {"status":"starting"}
//	HTTP 503  {"status":"stopping"}
//
// Neither runs a dependency check. "stopping" in particular must be immediate:
// it is what deregisters the pod from its load balancer, and waiting up to
// HealthTimeout on a dependency first would keep traffic arriving for exactly
// as long as the drain needs it to stop.
//
// Otherwise every dependency is checked concurrently under one HealthTimeout
// budget — the database (all distinct adapters the registry resolves to) plus
// each Config.ReadinessChecks entry:
//
//	HTTP 200  {"status":"ok"}
//	HTTP 503  {"status":"not_ready"}
//
// The per-dependency results are withheld unless
// Config.Probes.PublishReadinessChecks is set, since they name what the
// application depends on and which part is currently failing:
//
//	HTTP 503  {"status":"not_ready", "checks":{"db":"ok","broker":"error"}}
//
// Concurrent requests share one run of the checks — see probeFlight — so the
// endpoint cannot be used to amplify traffic against the dependencies it
// reports on.
//
// Unlike /health, this does not consult Config.HealthCheckDB: readiness that
// skips its dependencies reports nothing worth probing. Error text is logged,
// never echoed.
func readyHandler(cfg *Config, reg *Registry, phase func() readinessPhase) http.HandlerFunc {
	// One flight per server, not per process: two servers in the same binary
	// have different dependencies to report on.
	var flight probeFlight[map[string]string]

	return func(w http.ResponseWriter, r *http.Request) {
		switch phase() {
		case phaseStarting:
			writeProbe(w, http.StatusServiceUnavailable, probeBody{Status: "starting"})
			return
		case phaseStopping:
			writeProbe(w, http.StatusServiceUnavailable, probeBody{Status: "stopping"})
			return
		}

		ctx := r.Context()
		if cfg.HealthTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.HealthTimeout)
			defer cancel()
		}

		checks := flight.do(func() map[string]string {
			return runReadinessChecks(ctx, cfg, reg)
		})

		status, code := probeOK, http.StatusOK
		for _, result := range checks {
			if result == probeError {
				status, code = "not_ready", http.StatusServiceUnavailable
				break
			}
		}

		// The status code always answers the orchestrator's question. The
		// per-dependency map names what this application is built on and which
		// part of it is failing, so it is published only on request.
		body := probeBody{Status: status}
		if cfg.Probes.PublishReadinessChecks {
			body.Checks = checks
		}
		writeProbe(w, code, body)
	}
}

// runReadinessChecks runs the database check and every configured check
// concurrently on one budget, and returns their outcomes by name.
func runReadinessChecks(ctx context.Context, cfg *Config, reg *Registry) map[string]string {
	results := make(map[string]string, len(cfg.ReadinessChecks)+1)
	var mu sync.Mutex
	var wg sync.WaitGroup

	record := func(name, result string) {
		mu.Lock()
		defer mu.Unlock()
		results[name] = result
	}

	wg.Go(func() {
		record(readinessDBCheck, pingAdapters(ctx, cfg, reg))
	})

	for _, check := range cfg.ReadinessChecks {
		wg.Go(func() {
			if err := runReadinessCheck(ctx, check); err != nil {
				cfg.logger().Error("readiness: check failed",
					slog.String("check", check.Name),
					slog.String("error", err.Error()))
				record(check.Name, probeError)
				return
			}
			record(check.Name, probeOK)
		})
	}

	wg.Wait()
	return results
}

// runReadinessCheck calls one application check and converts a panic into a
// failed check. The checks run on their own goroutines, out of reach of the
// router's PanicRecoverer, so an unrecovered panic in application code would
// take the whole process down from a probe request.
func runReadinessCheck(ctx context.Context, check ReadinessCheck) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("readiness check %q panicked: %v", check.Name, r)
		}
	}()
	return check.Check(ctx)
}

// pingAdapters reports the database outcome across every distinct adapter the
// registry resolves to. One unreachable adapter fails the check: a model whose
// database is down cannot serve traffic.
//
// Shared by {prefix}/ready and by {prefix}/health when HealthCheckDB is on, so
// the two cannot come to disagree about whether the database is reachable. That
// is also why the log line names neither: with concurrent requests sharing one
// ping, whichever endpoint opened the flight is not the interesting fact.
//
// A panicking Ping is a failed check, not a crash. Ping is third-party code —
// any adapter may implement it — and on the readiness path this runs on its own
// goroutine, out of reach of the router's PanicRecoverer, so an unrecovered
// panic took the whole process down from an unauthenticated probe request. Same
// reasoning as runReadinessCheck, which already guarded the application's own
// checks but not the framework's.
func pingAdapters(ctx context.Context, cfg *Config, reg *Registry) (result string) {
	defer func() {
		if r := recover(); r != nil {
			cfg.logger().Error("probe: db ping panicked",
				slog.Any("panic", r))
			result = probeError
		}
	}()
	return pingAdaptersUnguarded(ctx, cfg, reg)
}

func pingAdaptersUnguarded(ctx context.Context, cfg *Config, reg *Registry) string {
	adapters := distinctAdapters(cfg, reg)
	if len(adapters) == 0 {
		return probeUnknown
	}

	pingable := false
	for _, a := range adapters {
		p, ok := a.(Pinger)
		if !ok {
			continue
		}
		pingable = true
		if err := p.Ping(ctx); err != nil {
			cfg.logger().Error("probe: db ping failed",
				slog.String("error", err.Error()))
			return probeError
		}
	}
	if !pingable {
		return probeUnknown
	}
	return probeOK
}

// validateReadinessChecks panics on a check the readiness endpoint could not
// report honestly — an anonymous check, one with no function to call, or a name
// that would overwrite another check's result in the response body. This is a
// programming error in the application's own configuration, caught once when
// the router is built rather than on every probe request.
func validateReadinessChecks(checks []ReadinessCheck) {
	seen := make(map[string]bool, len(checks))
	for i, check := range checks {
		switch {
		case check.Name == "":
			panic(fmt.Sprintf("maniflex: Config.ReadinessChecks[%d] must have a Name", i))
		case check.Check == nil:
			panic(fmt.Sprintf("maniflex: Config.ReadinessChecks[%d] (%q) must have a Check function", i, check.Name))
		case check.Name == readinessDBCheck:
			panic(fmt.Sprintf("maniflex: Config.ReadinessChecks[%d] uses %q, a name reserved for the framework's database check", i, readinessDBCheck))
		case seen[check.Name]:
			panic(fmt.Sprintf("maniflex: Config.ReadinessChecks[%d] has a duplicate Name %q", i, check.Name))
		}
		seen[check.Name] = true
	}
}
