package maniflex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
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
//	HTTP 200  {"status":"ok",        "checks":{"db":"ok","broker":"ok"}}
//	HTTP 503  {"status":"not_ready", "checks":{"db":"ok","broker":"error"}}
//
// Unlike /health, this does not consult Config.HealthCheckDB: readiness that
// skips its dependencies reports nothing worth probing. Error text is logged,
// never echoed.
func readyHandler(cfg *Config, reg *Registry, phase func() readinessPhase) http.HandlerFunc {
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

		checks := runReadinessChecks(ctx, cfg, reg)

		status, code := probeOK, http.StatusOK
		for _, result := range checks {
			if result == probeError {
				status, code = "not_ready", http.StatusServiceUnavailable
				break
			}
		}
		writeProbe(w, code, probeBody{Status: status, Checks: checks})
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
func pingAdapters(ctx context.Context, cfg *Config, reg *Registry) string {
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
			cfg.logger().Error("readiness: db ping failed",
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
