package e2e

// health_test.go tests the GET /health endpoint's DB-ping behaviour:
//   - Without HealthCheckDB the endpoint always returns 200 {"status":"ok"}
//   - With HealthCheckDB=true and a reachable DB it returns 200 {"status":"ok","db":"ok"}
//   - With HealthCheckDB=true and an unreachable DB it returns 503 {"status":"degraded",...}
//   - The HealthTimeout bounds the ping duration
//   - Adapters that do not implement Pinger degrade silently to 200
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestHealthCheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maniflex"
	"maniflex/db/sqlcore"
	"maniflex/db/sqlite"
	"maniflex/tests/e2e/testutil"
)

func TestHealthCheck(t *testing.T) {
	t.Parallel()

	// ── Default behaviour (HealthCheckDB = false) ─────────────────────────────

	t.Run("default_returns_200_ok", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/health").AssertStatus(http.StatusOK)
	})

	t.Run("default_body_has_status_ok", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/health").AssertJSON(func(body map[string]any) {
			if body["status"] != "ok" {
				t.Errorf("default health body: got %v, want {status:ok}", body)
			}
		})
	})

	t.Run("default_has_no_db_field", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/health").AssertJSON(func(body map[string]any) {
			if _, has := body["db"]; has {
				t.Errorf("default health must not include 'db' field, got %v", body)
			}
		})
	})

	t.Run("default_content_type_is_json", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/health")
		ct := resp.Header.Get("Content-Type")
		if ct == "" || ct[:16] != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", ct)
		}
	})

	// ── HealthCheckDB = true, DB reachable ────────────────────────────────────

	t.Run("db_check_enabled_returns_200_when_db_healthy", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{checkDB: true, pingErr: nil})
		srv.GET("/health").AssertStatus(http.StatusOK)
	})

	t.Run("db_check_enabled_body_has_db_ok", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{checkDB: true, pingErr: nil})
		srv.GET("/health").AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "db", body["db"], "ok")
		})
	})

	// ── HealthCheckDB = true, DB unreachable ──────────────────────────────────

	t.Run("db_check_enabled_returns_503_when_db_down", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{
			checkDB: true,
			pingErr: context.DeadlineExceeded, // simulate unreachable DB
		})
		srv.GET("/health").AssertStatus(http.StatusServiceUnavailable)
	})

	t.Run("db_down_body_has_status_degraded", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{
			checkDB: true,
			pingErr: context.DeadlineExceeded,
		})
		srv.GET("/health").AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "degraded")
			testutil.AssertEqual(t, "db field", body["db"], "error")
		})
	})

	t.Run("db_down_body_omits_raw_driver_error", func(t *testing.T) {
		// Roadmap §11B.1: the raw driver message is logged, not echoed back
		// to the wire, so health responses can't leak DSN fragments. The
		// 503 body must NOT contain an `error` field.
		t.Parallel()
		srv := newHealthServer(t, healthOptions{
			checkDB: true,
			pingErr: context.DeadlineExceeded,
		})
		srv.GET("/health").AssertJSON(func(body map[string]any) {
			if _, present := body["error"]; present {
				t.Errorf("503 body must not include the raw driver error, got: %v", body)
			}
			testutil.AssertEqual(t, "status", body["status"], "degraded")
			testutil.AssertEqual(t, "db", body["db"], "error")
		})
	})

	t.Run("db_down_content_type_is_json", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{
			checkDB: true,
			pingErr: context.DeadlineExceeded,
		})
		resp := srv.GET("/health")
		ct := resp.Header.Get("Content-Type")
		if ct == "" || ct[:16] != "application/json" {
			t.Errorf("503 Content-Type: got %q, want application/json", ct)
		}
	})

	// ── HealthTimeout ─────────────────────────────────────────────────────────

	t.Run("health_timeout_applied_to_ping", func(t *testing.T) {
		t.Parallel()
		// Mock adapter whose Ping blocks until its context is cancelled.
		// With a very short HealthTimeout the handler must return quickly.
		hangDone := make(chan struct{})
		hangAdapter := &hangingPinger{done: hangDone}

		server := maniflex.New(maniflex.Config{
			PathPrefix:    "/api",
			AutoMigrate:   false,
			HealthCheckDB: true,
			HealthTimeout: 50 * time.Millisecond,
		})
		server.MustRegister(testutil.DefaultModels()...)

		// Use a real SQLite adapter for CRUD, but swap it for a hanging pinger
		// so only the health endpoint is affected.
		realDB, err := sqlite.Open(":memory:", server.Registry())
		if err != nil {
			t.Fatalf("sqlite open: %v", err)
		}
		t.Cleanup(func() { realDB.Close() })
		server.SetDB(hangAdapter.wrap(realDB))

		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)

		start := time.Now()
		resp, err := http.Get(ts.URL + "/api/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		resp.Body.Close()
		elapsed := time.Since(start)

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("hanging ping must return 503, got %d", resp.StatusCode)
		}
		// The handler must return within HealthTimeout + generous slack.
		if elapsed > 2*time.Second {
			t.Errorf("health handler took %s, must return within ~50ms", elapsed)
		}
		close(hangDone) // unblock the goroutine
	})

	t.Run("health_timeout_default_is_3s_when_check_enabled", func(t *testing.T) {
		t.Parallel()
		// Construct Config directly to check that applyDefaults() fills HealthTimeout.
		cfg := &maniflex.Config{
			PathPrefix:    "/api",
			HealthCheckDB: true,
			HealthTimeout: 0, // not set
		}
		cfg.ApplyDefaults()
		testutil.AssertEqual(t, "default HealthTimeout", cfg.HealthTimeout, 3*time.Second)
	})

	t.Run("health_timeout_zero_when_check_disabled", func(t *testing.T) {
		t.Parallel()
		cfg := &maniflex.Config{
			PathPrefix:    "/api",
			HealthCheckDB: false, // disabled
			HealthTimeout: 0,
		}
		cfg.ApplyDefaults()
		// HealthTimeout stays 0 when HealthCheckDB is false — no default applied.
		testutil.AssertEqual(t, "HealthTimeout stays 0", cfg.HealthTimeout, time.Duration(0))
	})

	// ── Adapter without Pinger degrades gracefully ─────────────────────────────

	t.Run("adapter_without_ping_returns_200_with_db_unknown", func(t *testing.T) {
		t.Parallel()
		// noPingAdapter wraps a real DB adapter but does not implement Pinger.
		srv := newHealthServer(t, healthOptions{
			checkDB:  true,
			noPinger: true,
		})
		resp := srv.GET("/health")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "db unknown", body["db"], "unknown")
		})
	})

	// ── Real SQLite adapter satisfies Pinger ──────────────────────────────────

	t.Run("real_sqlite_adapter_passes_ping", func(t *testing.T) {
		t.Parallel()
		srv := newHealthServer(t, healthOptions{
			checkDB:    true,
			realSQLite: true,
		})
		resp := srv.GET("/health")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			testutil.AssertEqual(t, "status", body["status"], "ok")
			testutil.AssertEqual(t, "db ok", body["db"], "ok")
		})
	})

	// ── Health endpoint bypasses pipeline auth ────────────────────────────────

	t.Run("health_check_bypasses_auth_middleware", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "no access")
					return nil
				})
			},
		})
		// Health endpoint is a plain chi handler — must return 200 regardless.
		srv.GET("/health").AssertStatus(http.StatusOK)
	})

	// ── Per-model adapter routing is covered (11B.1 / H1) ────────────────────

	t.Run("per_model_adapter_failure_reported_as_degraded", func(t *testing.T) {
		// Roadmap §11B.1: when a model resolves to a per-model adapter
		// distinct from Config.DB, the health check must ping it too.
		// Pre-fix the handler only pinged cfg.DB and reported "ok" even
		// when an orders DB was unreachable.
		t.Parallel()

		server := maniflex.New(maniflex.Config{
			PathPrefix:    "/api",
			AutoMigrate:   false,
			HealthCheckDB: true,
			HealthTimeout: time.Second,
		})

		// Global DB (healthy).
		globalReal, err := sqlite.Open(":memory:", server.Registry())
		if err != nil {
			t.Fatalf("global sqlite open: %v", err)
		}
		t.Cleanup(func() { globalReal.Close() })

		// Per-model DB (broken — Ping returns an error).
		brokenReal, err := sqlite.Open(":memory:", server.Registry())
		if err != nil {
			t.Fatalf("broken sqlite open: %v", err)
		}
		t.Cleanup(func() { brokenReal.Close() })
		brokenAdapter := &mockPingAdapter{DBAdapter: brokenReal, pingErr: context.DeadlineExceeded}

		server.MustRegister(testutil.Tag{}, maniflex.ModelConfig{Adapter: brokenAdapter})
		server.MustRegister(testutil.User{}, testutil.Post{}, testutil.Comment{})
		server.SetDB(globalReal)

		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)

		resp, err := http.Get(ts.URL + "/api/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("broken per-model adapter must report 503, got %d", resp.StatusCode)
		}
	})

	// ── 503 on first check, 200 on recovery ───────────────────────────────────

	t.Run("health_reflects_db_recovery", func(t *testing.T) {
		t.Parallel()
		// Adapter that alternates between failing and passing pings.
		var failPing = true
		adapter := &togglePinger{shouldFail: &failPing}

		server := maniflex.New(maniflex.Config{
			PathPrefix:    "/api",
			AutoMigrate:   false,
			HealthCheckDB: true,
			HealthTimeout: time.Second,
		})
		server.MustRegister(testutil.DefaultModels()...)

		realDB, err := sqlite.Open(":memory:", server.Registry())
		if err != nil {
			t.Fatalf("sqlite open: %v", err)
		}
		t.Cleanup(func() { realDB.Close() })
		server.SetDB(adapter.wrap(realDB))

		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)

		getHealth := func() int {
			resp, err := http.Get(ts.URL + "/api/health")
			if err != nil {
				t.Fatalf("GET /health: %v", err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}

		// First check: DB is "down"
		if code := getHealth(); code != http.StatusServiceUnavailable {
			t.Errorf("first check: got %d, want 503", code)
		}

		// DB "recovers"
		failPing = false

		// Second check: DB is back up
		if code := getHealth(); code != http.StatusOK {
			t.Errorf("second check after recovery: got %d, want 200", code)
		}
	})
}

// ── Test infrastructure ───────────────────────────────────────────────────────

type healthOptions struct {
	checkDB    bool
	pingErr    error // nil = ping succeeds; non-nil = ping returns this error
	noPinger   bool  // adapter does not implement Pinger
	realSQLite bool  // use the real sqlite adapter (satisfies Pinger)
}

// newHealthServer builds an httptest.Server whose health behaviour is
// controlled by healthOptions.
func newHealthServer(t *testing.T, opts healthOptions) *testutil.Server {
	t.Helper()
	timeout := time.Second
	if !opts.checkDB {
		timeout = 0
	}
	return testutil.NewServer(t, testutil.Options{
		HealthCheckDB: opts.checkDB,
		HealthTimeout: timeout,
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			real, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			if opts.realSQLite {
				return real, nil
			}
			if opts.noPinger {
				return &noPingAdapter{DBAdapter: real}, nil
			}
			return &mockPingAdapter{DBAdapter: real, pingErr: opts.pingErr}, nil
		},
	})
}

// mockPingAdapter wraps a real adapter and replaces Ping with a controllable error.
type mockPingAdapter struct {
	maniflex.DBAdapter
	pingErr error
}

func (m *mockPingAdapter) Ping(_ context.Context) error { return m.pingErr }

// noPingAdapter wraps a real adapter but does NOT implement Pinger.
// Used to test graceful degradation.
type noPingAdapter struct {
	maniflex.DBAdapter
}

// hangingPinger is an adapter whose Ping blocks until its context is cancelled.
type hangingPinger struct {
	inner maniflex.DBAdapter
	done  chan struct{}
}

func (a *hangingPinger) Raw(ctx context.Context, query string, args ...any) maniflex.RawResult {
	return sqlcore.RawSqlResult{}
}

func (h *hangingPinger) wrap(inner maniflex.DBAdapter) *hangingPinger {
	h.inner = inner
	return h
}

func (h *hangingPinger) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		return nil
	}
}

// Delegate all DBAdapter methods to the wrapped adapter.
func (h *hangingPinger) AutoMigrate(ctx context.Context, reg maniflex.RegistryAccessor) error {
	return h.inner.AutoMigrate(ctx, reg)
}
func (h *hangingPinger) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	return h.inner.BeginTx(ctx, opts)
}
func (h *hangingPinger) FindByID(ctx context.Context, m *maniflex.ModelMeta, id string, q *maniflex.QueryParams) (any, error) {
	return h.inner.FindByID(ctx, m, id, q)
}
func (h *hangingPinger) FindByIDForUpdate(ctx context.Context, m *maniflex.ModelMeta, id string) (any, error) {
	return h.inner.FindByIDForUpdate(ctx, m, id)
}
func (h *hangingPinger) FindMany(ctx context.Context, m *maniflex.ModelMeta, q *maniflex.QueryParams) ([]any, int64, error) {
	return h.inner.FindMany(ctx, m, q)
}
func (h *hangingPinger) Create(ctx context.Context, m *maniflex.ModelMeta, record any) (any, error) {
	return h.inner.Create(ctx, m, record)
}
func (h *hangingPinger) Update(ctx context.Context, m *maniflex.ModelMeta, id string, record any, present map[string]struct{}) (any, error) {
	return h.inner.Update(ctx, m, id, record, present)
}
func (h *hangingPinger) Delete(ctx context.Context, m *maniflex.ModelMeta, id string) error {
	return h.inner.Delete(ctx, m, id)
}
func (h *hangingPinger) Close() error { return h.inner.Close() }

// togglePinger alternates ping success/failure via a shared bool pointer.
type togglePinger struct {
	inner      maniflex.DBAdapter
	shouldFail *bool
}

func (p *togglePinger) wrap(inner maniflex.DBAdapter) *togglePinger {
	p.inner = inner
	return p
}

func (p *togglePinger) Ping(_ context.Context) error {
	if *p.shouldFail {
		return context.DeadlineExceeded
	}
	return nil
}

func (a *togglePinger) Raw(ctx context.Context, query string, args ...any) maniflex.RawResult {
	return sqlcore.RawSqlResult{}
}

func (p *togglePinger) AutoMigrate(ctx context.Context, reg maniflex.RegistryAccessor) error {
	return p.inner.AutoMigrate(ctx, reg)
}
func (p *togglePinger) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	return p.inner.BeginTx(ctx, opts)
}
func (p *togglePinger) FindByID(ctx context.Context, m *maniflex.ModelMeta, id string, q *maniflex.QueryParams) (any, error) {
	return p.inner.FindByID(ctx, m, id, q)
}
func (p *togglePinger) FindByIDForUpdate(ctx context.Context, m *maniflex.ModelMeta, id string) (any, error) {
	return p.inner.FindByIDForUpdate(ctx, m, id)
}
func (p *togglePinger) FindMany(ctx context.Context, m *maniflex.ModelMeta, q *maniflex.QueryParams) ([]any, int64, error) {
	return p.inner.FindMany(ctx, m, q)
}
func (p *togglePinger) Create(ctx context.Context, m *maniflex.ModelMeta, record any) (any, error) {
	return p.inner.Create(ctx, m, record)
}
func (p *togglePinger) Update(ctx context.Context, m *maniflex.ModelMeta, id string, record any, present map[string]struct{}) (any, error) {
	return p.inner.Update(ctx, m, id, record, present)
}
func (p *togglePinger) Delete(ctx context.Context, m *maniflex.ModelMeta, id string) error {
	return p.inner.Delete(ctx, m, id)
}
func (p *togglePinger) Close() error { return p.inner.Close() }

// Ensure the mock types compile correctly.
var (
	_ maniflex.DBAdapter = (*mockPingAdapter)(nil)
	_ maniflex.DBAdapter = (*noPingAdapter)(nil)
	_ maniflex.DBAdapter = (*hangingPinger)(nil)
	_ maniflex.DBAdapter = (*togglePinger)(nil)
)
