package e2e

// R1 — tenancy did not survive Actions. db.Tenancy registers on the DB step,
// which an Action's pipeline (Auth → middleware → handler → Response) skips, so
// the filter it appends to ctx.Query had no reader and the action's DB access
// ran unscoped. Nothing warned: the ineffective-registration scan only fires for
// a middleware whose operation filter names *only* skipped steps, and the
// idiomatic registration names none.
//
// db.TenancyAction / db.ForceFilterAction set an ActionScope instead. Every DB
// path reachable from a handler either applies it or refuses to run — these
// tests pin both halves, because a scope that covers the convenient paths and
// leaks through the rest is a guarantee in the docs and not in the code.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// scopeProbe is what each action reports back, so a test asserts on what the
// handler actually saw rather than on a status code alone.
type scopeProbe struct {
	Count int    `json:"count"`
	Err   string `json:"err"`
	Title string `json:"title"`
}

// actionScopeSrv mounts one action per DB path, each scoped by X-Org, so a
// single server exercises every enforcement point.
func actionScopeSrv(t *testing.T, handlers map[string]func(*maniflex.ServerContext) error) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{Article{}},
		Middleware: func(s *maniflex.Server) {
			for path, h := range handlers {
				s.Action(maniflex.ActionConfig{
					Method: http.MethodPost,
					Path:   "/probe/" + path,
					Middleware: []maniflex.MiddlewareFunc{
						dbmw.TenancyAction("org_id", func(ctx *maniflex.ServerContext) string {
							return ctx.Request.Header.Get("X-Org")
						}),
					},
					Handler: h,
				})
			}
		},
	})
}

func reply(ctx *maniflex.ServerContext, p scopeProbe) error {
	ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: p}
	return nil
}

// seedTwoTenants puts one Article in tenant-a and one in tenant-b, returning
// tenant-a's id. It writes through the unscoped CRUD routes so the fixture does
// not depend on the thing under test.
func seedTwoTenants(t *testing.T, srv *testutil.Server) string {
	t.Helper()
	idA := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "a-doc", "body": "B", "status": "draft", "org_id": "tenant-a"}))
	srv.POST("/articles",
		map[string]any{"title": "b-doc", "body": "B", "status": "draft", "org_id": "tenant-b"}).
		AssertStatus(http.StatusCreated)
	return idA
}

// ── enforced paths ────────────────────────────────────────────────────────────

func TestActionScope_GetModelListIsScoped(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"list": func(ctx *maniflex.ServerContext) error {
			rows, err := ctx.GetModel("Article").List(nil)
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Count: len(rows)})
		},
	})
	seedTwoTenants(t, srv)

	got := srv.POST("/probe/list", nil, map[string]string{"X-Org": "tenant-a"})
	got.AssertStatus(http.StatusOK)
	if n := got.Data()["count"]; n != float64(1) {
		t.Errorf("ctx.GetModel(...).List inside a scoped action saw %v rows, want 1 — the "+
			"action's list is not scoped to the caller's tenant", n)
	}
}

func TestActionScope_GetModelReadIsScoped(t *testing.T) {
	var target string
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"read": func(ctx *maniflex.ServerContext) error {
			row, err := ctx.GetModel("Article").Read(target)
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Title: row["title"].(string)})
		},
	})
	target = seedTwoTenants(t, srv) // tenant-a's row

	ok := srv.POST("/probe/read", nil, map[string]string{"X-Org": "tenant-a"})
	if got := ok.Data()["title"]; got != "a-doc" {
		t.Errorf("owner read title=%v, want %q", got, "a-doc")
	}

	cross := srv.POST("/probe/read", nil, map[string]string{"X-Org": "tenant-b"})
	if e := cross.Data()["err"]; e == "" {
		t.Errorf("a cross-tenant read inside a scoped action returned the row (title=%v) — "+
			"the scope is not reaching ctx.GetModel(...).Read", cross.Data()["title"])
	}
}

// The write half: Update/Delete are keyed by id alone, so the scope can only be
// held by looking the record up through it first.
func TestActionScope_GetModelWritesAreScoped(t *testing.T) {
	var target string
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"update": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.GetModel("Article").Update(target, map[string]any{"title": "PWNED"})
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{})
		},
		"delete": func(ctx *maniflex.ServerContext) error {
			if err := ctx.GetModel("Article").Delete(target); err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{})
		},
	})
	target = seedTwoTenants(t, srv)

	if e := srv.POST("/probe/update", nil, map[string]string{"X-Org": "tenant-b"}).
		Data()["err"]; e == "" {
		t.Error("a cross-tenant Update inside a scoped action succeeded — an action can " +
			"write another tenant's row")
	}
	if got := srv.GET("/articles/" + target).Data()["title"]; got != "a-doc" {
		t.Errorf("title = %v, want %q — the cross-tenant Update was reported as failing but "+
			"still wrote", got, "a-doc")
	}

	if e := srv.POST("/probe/delete", nil, map[string]string{"X-Org": "tenant-b"}).
		Data()["err"]; e == "" {
		t.Error("a cross-tenant Delete inside a scoped action succeeded")
	}
	srv.GET("/articles/" + target).AssertStatus(http.StatusOK) // survives
}

// In-scope writes must keep working, or the scope has simply broken actions.
func TestActionScope_InScopeWritesWork(t *testing.T) {
	var target string
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"update": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.GetModel("Article").Update(target, map[string]any{"title": "renamed"})
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{})
		},
	})
	target = seedTwoTenants(t, srv)

	got := srv.POST("/probe/update", nil, map[string]string{"X-Org": "tenant-a"})
	if e := got.Data()["err"]; e != "" {
		t.Fatalf("an in-scope Update was refused: %v", e)
	}
	if v := srv.GET("/articles/" + target).Data()["title"]; v != "renamed" {
		t.Errorf("title = %v, want %q — an in-scope Update was dropped", v, "renamed")
	}
}

// A create must be stamped with the scope, not merely permitted: a row created
// outside it would be invisible to the caller that created it, and a caller
// choosing the value is the placement the scope exists to prevent.
func TestActionScope_CreateIsStamped(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"create": func(ctx *maniflex.ServerContext) error {
			row, err := ctx.GetModel("Article").Create(map[string]any{
				"title": "new", "body": "B", "status": "draft",
				"org_id": "tenant-b", // the caller tries to plant it elsewhere
			})
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Title: row["org_id"].(string)})
		},
	})

	got := srv.POST("/probe/create", nil, map[string]string{"X-Org": "tenant-a"})
	got.AssertStatus(http.StatusOK)
	if v := got.Data()["title"]; v != "tenant-a" {
		t.Errorf("created row org_id = %v, want %q — a scoped action created a row in "+
			"another tenant's bucket", v, "tenant-a")
	}
}

// The typed generics never touch GetModel, so they are a separate enforcement
// point and an unscoped one would be a silent leak.
func TestActionScope_TypedGenericsAreScoped(t *testing.T) {
	var target string
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"list": func(ctx *maniflex.ServerContext) error {
			rows, err := maniflex.List[Article](ctx, nil)
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Count: len(rows)})
		},
		"read": func(ctx *maniflex.ServerContext) error {
			_, err := maniflex.Read[Article](ctx, target)
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{})
		},
		"delete": func(ctx *maniflex.ServerContext) error {
			if err := maniflex.Delete[Article](ctx, target); err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{})
		},
	})
	target = seedTwoTenants(t, srv)

	if n := srv.POST("/probe/list", nil, map[string]string{"X-Org": "tenant-a"}).
		Data()["count"]; n != float64(1) {
		t.Errorf("maniflex.List[Article] inside a scoped action saw %v rows, want 1", n)
	}
	if e := srv.POST("/probe/read", nil, map[string]string{"X-Org": "tenant-b"}).
		Data()["err"]; e == "" {
		t.Error("maniflex.Read[Article] returned another tenant's row inside a scoped action")
	}
	if e := srv.POST("/probe/delete", nil, map[string]string{"X-Org": "tenant-b"}).
		Data()["err"]; e == "" {
		t.Error("maniflex.Delete[Article] deleted another tenant's row inside a scoped action")
	}
	srv.GET("/articles/" + target).AssertStatus(http.StatusOK)
}

func TestActionScope_AggregateIsScoped(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"agg": func(ctx *maniflex.ServerContext) error {
			rows, err := ctx.Aggregate("Article", maniflex.AggregateQuery{
				Select: []maniflex.AggregateField{{Op: maniflex.AggCount, As: "n"}},
			})
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			n := 0
			if len(rows) == 1 {
				switch v := rows[0]["n"].(type) {
				case int64:
					n = int(v)
				case float64:
					n = int(v)
				}
			}
			return reply(ctx, scopeProbe{Count: n})
		},
	})
	seedTwoTenants(t, srv)

	got := srv.POST("/probe/agg", nil, map[string]string{"X-Org": "tenant-a"})
	if n := got.Data()["count"]; n != float64(1) {
		t.Errorf("ctx.Aggregate counted %v rows inside a scoped action, want 1 — an "+
			"unscoped aggregate leaks across tenants in summary form", n)
	}
}

// ── refused paths ─────────────────────────────────────────────────────────────

// The paths that cannot carry a filter must refuse, not run. This is what makes
// the guarantee structural rather than a documented hope.
func TestActionScope_UnscoppablePathsRefuse(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"raw_query": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.RawQuery("SELECT id FROM articles")
			return reply(ctx, scopeProbe{Err: errStr(err)})
		},
		"raw_exec": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.RawExec("UPDATE articles SET title = 'x'")
			return reply(ctx, scopeProbe{Err: errStr(err)})
		},
		"begin_tx": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.BeginTx(ctx.Ctx, nil)
			return reply(ctx, scopeProbe{Err: errStr(err)})
		},
		"search": func(ctx *maniflex.ServerContext) error {
			_, err := ctx.Search(maniflex.SearchOptions{Query: "doc"})
			return reply(ctx, scopeProbe{Err: errStr(err)})
		},
	})
	seedTwoTenants(t, srv)

	for _, path := range []string{"raw_query", "raw_exec", "begin_tx", "search"} {
		got := srv.POST("/probe/"+path, nil, map[string]string{"X-Org": "tenant-a"})
		e, _ := got.Data()["err"].(string)
		if e == "" {
			t.Errorf("ctx.%s ran unscoped inside a scoped action — the scope cannot be "+
				"applied to it, so it must refuse rather than return every tenant's rows", path)
			continue
		}
		if !contains(e, "Unscoped") {
			t.Errorf("%s refusal does not mention the ctx.Unscoped() escape, so it says what "+
				"is wrong but not what to do: %s", path, e)
		}
	}
}

// The refusals must not fire on an ordinary, unscoped action — every existing
// action in the world is one of those.
func TestActionScope_UnscopedActionUnaffected(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Article{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: http.MethodPost, Path: "/probe/raw",
				Handler: func(ctx *maniflex.ServerContext) error {
					rows, err := ctx.RawQuery("SELECT id FROM articles")
					if err != nil {
						return reply(ctx, scopeProbe{Err: err.Error()})
					}
					return reply(ctx, scopeProbe{Count: len(rows)})
				},
			})
		},
	})
	seedTwoTenants(t, srv)

	got := srv.POST("/probe/raw", nil)
	got.AssertStatus(http.StatusOK)
	if e := got.Data()["err"]; e != "" {
		t.Fatalf("ctx.RawQuery failed in an action with no scope: %v", e)
	}
	if n := got.Data()["count"]; n != float64(2) {
		t.Errorf("unscoped action saw %v rows, want 2 — the scope guard is firing on a "+
			"request that has no scope", n)
	}
}

// The escape has to work, or the refusal is a wall rather than a decision.
func TestActionScope_UnscopedEscapeWorks(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"raw": func(ctx *maniflex.ServerContext) error {
			rows, err := ctx.Unscoped().RawQuery("SELECT id FROM articles")
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Count: len(rows)})
		},
		"get_model": func(ctx *maniflex.ServerContext) error {
			rows, err := ctx.Unscoped().GetModel("Article").List(nil)
			if err != nil {
				return reply(ctx, scopeProbe{Err: err.Error()})
			}
			return reply(ctx, scopeProbe{Count: len(rows)})
		},
	})
	seedTwoTenants(t, srv)

	for _, path := range []string{"raw", "get_model"} {
		got := srv.POST("/probe/"+path, nil, map[string]string{"X-Org": "tenant-a"})
		if e := got.Data()["err"]; e != "" {
			t.Fatalf("ctx.Unscoped().%s was refused: %v", path, e)
		}
		if n := got.Data()["count"]; n != float64(2) {
			t.Errorf("ctx.Unscoped().%s saw %v rows, want 2 (both tenants) — the escape is "+
				"still applying the scope", path, n)
		}
	}
}

// TenancyAction refuses a caller whose tenant cannot be determined, rather than
// leaving them unscoped — the case where running unscoped would be worst.
func TestActionScope_UnidentifiableTenantRefused(t *testing.T) {
	srv := actionScopeSrv(t, map[string]func(*maniflex.ServerContext) error{
		"list": func(ctx *maniflex.ServerContext) error {
			rows, _ := ctx.GetModel("Article").List(nil)
			return reply(ctx, scopeProbe{Count: len(rows)})
		},
	})
	seedTwoTenants(t, srv)

	srv.POST("/probe/list", nil).AssertStatus(http.StatusForbidden) // no X-Org
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
