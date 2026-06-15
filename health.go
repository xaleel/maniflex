package maniflex

import (
	"context"
	"encoding/json"
	"log/slog"
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
func healthHandler(cfg *Config, reg *Registry) http.HandlerFunc {
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

		adapters := distinctAdapters(cfg, reg)
		if len(adapters) == 0 {
			// No adapters configured at all (no global, no per-model). Treat as
			// "unknown" rather than degraded so a server still in bootstrap
			// doesn't fail liveness probes.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "ok",
				"db":     "unknown",
			})
			return
		}

		anySupportsPing := false
		for _, a := range adapters {
			p, ok := a.(Pinger)
			if !ok {
				continue
			}
			anySupportsPing = true
			if err := p.Ping(ctx); err != nil {
				cfg.logger().Error("health: db ping failed",
					slog.String("error", err.Error()))
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"status": "degraded",
					"db":     "error",
				})
				return
			}
		}

		if !anySupportsPing {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "ok",
				"db":     "unknown",
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"db":     "ok",
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
	if reg != nil {
		for _, m := range reg.All() {
			add(m.ResolveAdapter(cfg.DB))
		}
	} else {
		add(cfg.DB)
	}
	return out
}
