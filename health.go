package maniflex

import (
	"context"
	"encoding/json"
	"net/http"
)

// Pinger is satisfied by any DB adapter that exposes a Ping method.
// It is deliberately narrow — only the health handler uses it, so we avoid
// adding Ping to the full DBAdapter interface (which would break all custom
// adapters written against the previous interface).
//
// *sqlcore.Adapter satisfies Pinger automatically because it wraps *sql.DB,
// which has PingContext. Custom adapters that do not embed *sql.DB can add:
//
//	func (a *MyAdapter) Ping(ctx context.Context) error { return a.db.PingContext(ctx) }
type Pinger interface {
	Ping(ctx context.Context) error
}

// healthHandler returns an http.HandlerFunc for GET /health.
//
// When cfg.HealthCheckDB is false (the default) it always returns:
//
//	HTTP 200  {"status":"ok"}
//
// When cfg.HealthCheckDB is true it pings every distinct adapter the
// registry resolves to (the global cfg.DB plus any per-model overrides)
// within cfg.HealthTimeout and returns one of:
//
//	HTTP 200  {"status":"ok",       "db":"ok"}
//	HTTP 503  {"status":"degraded", "db":"error"}
//
// Raw driver error messages are *not* echoed back to the client (they can
// leak DSN fragments). The full error is logged via cfg.Logger so operators
// can correlate.
//
// The adapter is tested for the Pinger interface at call time, not at
// construction, so the handler degrades gracefully if the adapter does not
// implement Ping: it returns "db":"unknown" rather than failing the check.
//
// The ping itself is pingAdapters, shared with GET {prefix}/ready — one
// endpoint disagreeing with the other about whether the database is reachable
// would be a bug with nowhere to hide. Concurrent requests share one ping (see
// probeFlight), so an unauthenticated probe cannot be used to amplify traffic
// against the database.
//
// The "db" key is not gated by Config.Probes.PublishReadinessChecks: it is a
// name the framework owns rather than one the application chose, so unlike
// readiness it describes no topology.
func healthHandler(cfg *Config, reg *Registry) http.HandlerFunc {
	var flight probeFlight[string]

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Fast path: DB check disabled — static response, no I/O.
		if !cfg.HealthCheckDB {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.HealthTimeout)
		defer cancel()

		db := flight.do(func() string { return pingAdapters(ctx, cfg, reg) })

		if db == probeError {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "degraded",
				"db":     probeError,
			})
			return
		}

		// "unknown" covers both no adapter configured at all and an adapter
		// that cannot be pinged. Neither is degraded: a server still in
		// bootstrap, or one whose adapter does not implement Pinger, has not
		// told us anything is wrong.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"db":     db,
		})
	}
}

// distinctAdapters returns the set of unique adapters that the registry
// resolves to — Config.DB plus any per-model ModelConfig.Adapter overrides.
// Uses pointer identity for deduplication (all known adapters are pointer
// types). Nil adapters are skipped.
func distinctAdapters(cfg *Config, reg *Registry) []DBAdapter {
	seen := make(map[DBAdapter]bool)
	var out []DBAdapter
	add := func(a DBAdapter) {
		if a == nil || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, a)
	}
	// Always include the global adapter, even when no model is registered yet —
	// otherwise a bootstrapping server with a reachable DB reports db:"unknown".
	add(cfg.DB)
	if reg != nil {
		for _, m := range reg.All() {
			add(m.ResolveAdapter(cfg.DB))
		}
	}
	return out
}
