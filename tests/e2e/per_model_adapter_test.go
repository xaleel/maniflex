package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestPerModelAdapter covers roadmap item 3A.2 — per-model DB adapter routing.
//
// Run this group specifically:
//
//	go test ./tests/e2e/... -run TestPerModelAdapter
func TestPerModelAdapter(t *testing.T) {
	t.Parallel()

	t.Run("isolation_users_in_dbA_posts_in_dbB", func(t *testing.T) {
		t.Parallel()
		f := newSplitFixture(t, splitOpts{})

		userID := mustID(t, f.post("/api/users", map[string]any{
			"name": "alice", "email": "a@x.io", "password": "pw", "role": "admin",
		}))

		if got := rawCount(t, f.dbA, "SELECT count(*) FROM users WHERE id=?", userID); got != 1 {
			t.Errorf("users row in dbA: got %d, want 1", got)
		}
		if tableExists(t, f.dbB, "users") {
			t.Errorf("dbB unexpectedly has a 'users' table; AutoMigrate fan-out leaked")
		}
	})

	t.Run("automigrate_only_creates_routed_tables", func(t *testing.T) {
		t.Parallel()
		f := newSplitFixture(t, splitOpts{})

		if !tableExists(t, f.dbA, "users") {
			t.Errorf("dbA missing 'users' table")
		}
		if tableExists(t, f.dbA, "posts") {
			t.Errorf("dbA unexpectedly has 'posts' table")
		}
		if !tableExists(t, f.dbB, "posts") {
			t.Errorf("dbB missing 'posts' table")
		}
		if tableExists(t, f.dbB, "users") {
			t.Errorf("dbB unexpectedly has 'users' table")
		}
	})

	t.Run("fallback_to_global", func(t *testing.T) {
		t.Parallel()
		f := newSplitFixture(t, splitOpts{withComment: true})

		userResp := f.post("/api/users", map[string]any{
			"name": "bob", "email": "b@x.io", "password": "pw", "role": "editor",
		})
		userID := mustID(t, userResp)
		postResp := f.post("/api/posts", map[string]any{
			"title": "hello", "body": "x", "status": "draft", "user_id": userID,
		})
		postID := mustID(t, postResp)
		commentResp := f.post("/api/comments", map[string]any{
			"body": "hi", "post_id": postID, "user_id": userID,
		})
		if commentResp.StatusCode != http.StatusCreated {
			t.Fatalf("create comment: status %d body %s", commentResp.StatusCode, commentResp.Body)
		}

		// Comment is on the global adapter, not dbA/dbB.
		if !tableExists(t, f.global, "comments") {
			t.Errorf("global adapter missing 'comments' table")
		}
		if tableExists(t, f.dbA, "comments") || tableExists(t, f.dbB, "comments") {
			t.Errorf("comment table leaked to per-model adapters")
		}
	})

	t.Run("no_global_required_when_every_model_has_adapter", func(t *testing.T) {
		t.Parallel()

		dbA, err := sqlite.Open(":memory:", nil)
		if err != nil {
			t.Fatalf("open dbA: %v", err)
		}
		dbB, err := sqlite.Open(":memory:", nil)
		if err != nil {
			t.Fatalf("open dbB: %v", err)
		}
		t.Cleanup(func() { dbA.Close(); dbB.Close() })

		server := maniflex.New(maniflex.Config{
			PathPrefix: "/api",
		})
		server.MustRegister(
			testutil.User{}, maniflex.ModelConfig{Adapter: dbA},
			testutil.Post{}, maniflex.ModelConfig{Adapter: dbB},
		)

		if err := server.MigrateOnly(context.Background()); err != nil {
			t.Fatalf("MigrateOnly with nil Config.DB but per-model adapters: %v", err)
		}

		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)

		resp, err := ts.Client().Post(ts.URL+"/api/users", "application/json",
			marshalBody(t, map[string]any{
				"name": "x", "email": "x@x.io", "password": "pw", "role": "admin",
			}))
		if err != nil {
			t.Fatalf("POST users: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("POST users without global adapter: status %d", resp.StatusCode)
		}
	})

	t.Run("missing_adapter_for_unrouted_model_errors", func(t *testing.T) {
		t.Parallel()

		dbA, err := sqlite.Open(":memory:", nil)
		if err != nil {
			t.Fatalf("open dbA: %v", err)
		}
		t.Cleanup(func() { dbA.Close() })

		server := maniflex.New(maniflex.Config{})
		server.MustRegister(
			testutil.User{}, maniflex.ModelConfig{Adapter: dbA},
			testutil.Post{}, // no adapter, no Config.DB → error
		)
		if err := server.MigrateOnly(context.Background()); err == nil {
			t.Fatal("expected MigrateOnly to fail when an unrouted model has no global adapter, got nil")
		}
	})

	t.Run("begintx_routes_to_model_adapter", func(t *testing.T) {
		t.Parallel()

		// Service middleware on User opens a transaction, writes a row through
		// GetModel.Create, then rolls back. ctx.Model is User so the per-model
		// adapter (dbA) is used. The User must not be visible after rollback
		// and dbB must remain unaware of the users table.
		f := newSplitFixture(t, splitOpts{
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						ctx.Abort(http.StatusInternalServerError, "TX", err.Error())
						return nil
					}
					ctx.Tx = tx
					if _, err := ctx.GetModel("User").Create(map[string]any{
						"name": "rolledback", "email": "rb@x.io",
						"password": "pw", "role": "admin",
					}); err != nil {
						tx.Rollback()
						ctx.Abort(http.StatusInternalServerError, "CREATE", err.Error())
						return nil
					}
					if err := tx.Rollback(); err != nil {
						ctx.Abort(http.StatusInternalServerError, "RB", err.Error())
						return nil
					}
					ctx.Tx = nil
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})

		// Trigger the middleware by listing users.
		if resp := f.get("/api/users"); resp.StatusCode != http.StatusOK {
			t.Fatalf("list users: status %d body %s", resp.StatusCode, resp.Body)
		}

		if got := rawCount(t, f.dbA, "SELECT count(*) FROM users WHERE email=?", "rb@x.io"); got != 0 {
			t.Errorf("expected rollback to leave 0 rows in dbA, got %d", got)
		}
		if tableExists(t, f.dbB, "users") {
			t.Errorf("dbB must not host users table; BeginTx leaked to global")
		}
	})

	t.Run("batch_cross_adapter_rejected", func(t *testing.T) {
		t.Parallel()

		// Service middleware on User runs maniflex.Batch — first writes a User
		// (same adapter, OK), then attempts to write a Post (different
		// adapter, must error). The batch must roll back the User write.
		var batchErr error
		f := newSplitFixture(t, splitOpts{
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					batchErr = maniflex.Batch(ctx, func(b *maniflex.Batcher) error {
						if _, err := b.Create("User", map[string]any{
							"name": "u1", "email": "u1@x.io",
							"password": "pw", "role": "admin",
						}); err != nil {
							return fmt.Errorf("user create: %w", err)
						}
						_, err := b.Create("Post", map[string]any{
							"title": "p1", "body": "x", "status": "draft",
							"user_id": "00000000-0000-0000-0000-000000000000",
						})
						return err
					})
					if batchErr != nil {
						ctx.Abort(http.StatusConflict, "CROSS", batchErr.Error())
						return nil
					}
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})

		resp := f.get("/api/users")
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409, got %d body %s", resp.StatusCode, resp.Body)
		}
		if code := errorCode(t, resp.Body); code != "CROSS" {
			t.Errorf("error code: got %q want CROSS", code)
		}
		if batchErr == nil {
			t.Error("expected Batch to return an error")
		}
		if got := rawCount(t, f.dbA, "SELECT count(*) FROM users WHERE email=?", "u1@x.io"); got != 0 {
			t.Errorf("cross-adapter batch must roll back the user write on dbA, got %d", got)
		}
	})

	t.Run("rawquery_uses_model_adapter", func(t *testing.T) {
		t.Parallel()

		// Service middleware on Post calls RawQuery on the `posts` table.
		// That table only exists on dbB; if RawQuery had used the global
		// adapter, the call would fail with "no such table".
		var queryErr error
		var rowCount int
		f := newSplitFixture(t, splitOpts{
			middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					rows, err := ctx.RawQuery("SELECT count(*) AS c FROM posts")
					if err != nil {
						queryErr = err
						ctx.Abort(http.StatusInternalServerError, "RAW", err.Error())
						return nil
					}
					rowCount = len(rows)
					return next()
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpList))
			},
		})

		resp := f.get("/api/posts")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list posts: status %d body %s, queryErr=%v", resp.StatusCode, resp.Body, queryErr)
		}
		if queryErr != nil {
			t.Errorf("RawQuery should hit dbB where the posts table lives: %v", queryErr)
		}
		if rowCount != 1 {
			t.Errorf("expected RawQuery to return 1 count row, got %d", rowCount)
		}
	})
}

// ── fixture ──────────────────────────────────────────────────────────────────

type splitFixture struct {
	server           *maniflex.Server
	ts               *httptest.Server
	dbA, dbB, global maniflex.DBAdapter
	client           *http.Client
	t                *testing.T
}

type splitOpts struct {
	// withComment registers Comment on the global adapter.
	withComment bool
	// middleware is called after models are registered and before MigrateOnly.
	middleware func(*maniflex.Server)
}

func newSplitFixture(t *testing.T, opts splitOpts) *splitFixture {
	t.Helper()

	dbA, err := sqlite.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("open dbA: %v", err)
	}
	dbB, err := sqlite.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("open dbB: %v", err)
	}
	global, err := sqlite.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("open global: %v", err)
	}

	server := maniflex.New(maniflex.Config{
		PathPrefix: "/api",
		DB:         global,
	})

	regs := []any{
		testutil.User{}, maniflex.ModelConfig{Adapter: dbA},
		testutil.Post{}, maniflex.ModelConfig{Adapter: dbB},
	}
	if opts.withComment {
		regs = append(regs, testutil.Comment{})
	}
	server.MustRegister(regs...)

	if opts.middleware != nil {
		opts.middleware(server)
	}

	if err := server.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("MigrateOnly: %v", err)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		ts.Close()
		dbA.Close()
		dbB.Close()
		global.Close()
	})

	return &splitFixture{
		server: server, ts: ts,
		dbA: dbA, dbB: dbB, global: global,
		client: ts.Client(), t: t,
	}
}

func (f *splitFixture) post(path string, body any) *rawResp {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, marshalBody(f.t, body))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(f.t, f.client, req)
}

func (f *splitFixture) get(path string) *rawResp {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	return doRequest(f.t, f.client, req)
}

// ── http helpers ─────────────────────────────────────────────────────────────

type rawResp struct {
	StatusCode int
	Body       []byte
}

func doRequest(t *testing.T, c *http.Client, req *http.Request) *rawResp {
	t.Helper()
	r, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return &rawResp{StatusCode: r.StatusCode, Body: b}
}

func marshalBody(t *testing.T, body any) io.Reader {
	t.Helper()
	if body == nil {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

func mustID(t *testing.T, r *rawResp) string {
	t.Helper()
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d body %s", r.StatusCode, r.Body)
	}
	var env struct {
		Data struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &env); err != nil {
		t.Fatalf("decode id: %v body %s", err, r.Body)
	}
	if env.Data.ID == "" {
		t.Fatalf("no id in response body %s", r.Body)
	}
	return env.Data.ID
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// ── DB helpers ───────────────────────────────────────────────────────────────

func rawCount(t *testing.T, adapter maniflex.DBAdapter, query string, args ...any) int {
	t.Helper()
	rows, err := adapter.Raw(context.Background(), query, args...).Rows()
	if err != nil {
		t.Fatalf("raw count: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("raw count: no rows")
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("raw count: scan: %v", err)
	}
	return n
}

func tableExists(t *testing.T, adapter maniflex.DBAdapter, name string) bool {
	t.Helper()
	rows, err := adapter.Raw(context.Background(),
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Rows()
	if err != nil {
		t.Fatalf("table exists: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return false
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("table exists scan: %v", err)
	}
	return n > 0
}
