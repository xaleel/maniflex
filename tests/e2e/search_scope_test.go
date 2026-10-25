package e2e_test

// Audit AUTH-20: GET /search was unscoped, whatever the app had registered.
//
// It runs the trimmed pipeline — Auth → scope → handler → Response — and
// scopeMiddlewares drops a ProvidesScope registration whose own step the
// operation skips, because hoisting changes when a middleware runs and never
// whether it does. A row scope registered the conventional way, on the DB step,
// is therefore simply absent, and ctx.Search ignored ctx.Query.Filters even when
// a scope did reach it. So every caller got every tenant's hits, with the
// matched text in the snippet.
//
// Two halves are pinned here, and they are independent: that a scope registered
// where OpSearch runs now constrains the search SQL, and that a scope registered
// anywhere else is named at boot instead of failing silently.
//
//	go test ./tests/e2e/... -run TestSearchScope

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Two searchable models: one tenant-scoped, one with no tenant column at all —
// the second is how a forced filter naming a column a model lacks gets tested.
type scopedMemo struct {
	maniflex.BaseModel
	TenantID string `json:"tenant_id" db:"tenant_id"`
	Note     string `json:"note" db:"note" mfx:"searchable"`
}

type publicMemo struct {
	maniflex.BaseModel
	Note string `json:"note" db:"note" mfx:"searchable"`
}

func searchScopeModels() []any {
	return []any{
		scopedMemo{}, maniflex.ModelConfig{GlobalSearchable: true},
		publicMemo{}, maniflex.ModelConfig{GlobalSearchable: true},
	}
}

// searchScopeServer mounts both models behind a header-driven tenant identity and
// lets each test register its scope wherever it likes. logTo captures boot
// output when non-nil.
func searchScopeServer(t *testing.T, logTo *bytes.Buffer, reg func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	opts := testutil.Options{
		Models: searchScopeModels(),
		Middleware: func(s *maniflex.Server) {
			s.EnableGlobalSearch(maniflex.GlobalSearchConfig{MaxLimit: 20, DefaultLimit: 20})
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Auth = &maniflex.AuthInfo{
					UserID:   "u-" + ctx.Request.Header.Get("X-Tenant"),
					TenantID: ctx.Request.Header.Get("X-Tenant"),
					Claims:   map[string]any{},
				}
				return next()
			})
			reg(s)
		},
	}
	if logTo != nil {
		opts.Logger = slog.New(slog.NewTextHandler(logTo, nil))
	}
	return testutil.NewServer(t, opts)
}

func asTenant(id string) map[string]string {
	return map[string]string{"X-Tenant": id}
}

// tenantScope is the registration that works: on a step OpSearch runs, unscoped
// by model so it matches the synthetic "__search" model.
func tenantScope() maniflex.MiddlewareFunc {
	return dbmw.Tenancy("tenant_id", func(ctx *maniflex.ServerContext) string {
		return ctx.Auth.TenantID
	})
}

// seedTwoTenants writes one scoped memo per tenant and one unscoped public memo,
// all matching the same search term.
func seedTwoTenants(t *testing.T, s *testutil.Server) {
	t.Helper()
	s.POST("/scoped_memos", map[string]any{"note": "diagnosis hypertension"}, asTenant("acme")).
		AssertStatus(http.StatusCreated)
	s.POST("/scoped_memos", map[string]any{"note": "diagnosis diabetes"}, asTenant("globex")).
		AssertStatus(http.StatusCreated)
}

// ── (a) a scope registered where OpSearch runs now constrains the search ──────

func TestSearchScope_AuthStepScopeConstrainsSearch(t *testing.T) {
	t.Parallel()
	s := searchScopeServer(t, nil, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(tenantScope(), maniflex.ProvidesScope())
	})
	seedTwoTenants(t, s)

	hits := s.GET("/search?q=diagnosis", asTenant("acme")).DataList()
	if len(hits) != 1 {
		t.Fatalf("got %d hit(s), want 1 — the other tenant's row must not be reachable: %v", len(hits), hits)
	}
	// The snippet carries the matched text, so a stray hit leaks content and not
	// merely an id. Assert on it rather than on the count alone.
	first, _ := hits[0].(map[string]any)
	if snip, _ := first["snippet"].(string); !strings.Contains(snip, "hypertension") {
		t.Errorf("snippet %q is not the caller's own row", snip)
	}

	// MUST STILL WORK: the other tenant sees their own row, so the scope is a
	// scope and not a blanket refusal.
	other := s.GET("/search?q=diagnosis", asTenant("globex")).DataList()
	if len(other) != 1 {
		t.Errorf("globex got %d hit(s), want 1", len(other))
	}
}

// A forced filter naming a column a searchable model does not have must exclude
// that model, not pass it through. BuildFilterSQL renders an unresolvable field
// as a false predicate, which is the fail-closed direction; publicMemo has no
// tenant_id, so a tenant-scoped search returns none of it.
func TestSearchScope_ModelWithoutTheScopeColumnIsExcluded(t *testing.T) {
	t.Parallel()
	// Named to the read operations, so the create stamp does not reach
	// publicMemo — which has no tenant_id to stamp. Naming OpSearch is the
	// other registration shape that reaches the endpoint, and pinning it here
	// means ForOperation(OpSearch) on the Auth step is covered too.
	s := searchScopeServer(t, nil, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(tenantScope(), maniflex.ProvidesScope(),
			maniflex.ForOperation(maniflex.OpSearch, maniflex.OpList, maniflex.OpRead))
	})
	// Creates are outside the scope here, so the tenant is sent explicitly.
	s.POST("/scoped_memos",
		map[string]any{"note": "diagnosis hypertension", "tenant_id": "acme"}, asTenant("acme")).
		AssertStatus(http.StatusCreated)
	s.POST("/public_memos", map[string]any{"note": "diagnosis public"}, asTenant("acme")).
		AssertStatus(http.StatusCreated)

	// The caller's own scoped row is still found, so the scope did not become a
	// blanket exclusion.
	var sawScoped bool
	for _, h := range s.GET("/search?q=diagnosis", asTenant("acme")).DataList() {
		if m, _ := h.(map[string]any); m["model"] == "scopedMemo" {
			sawScoped = true
		}
	}
	if !sawScoped {
		t.Error("the caller's own scopedMemo row should still be a hit")
	}

	for _, h := range s.GET("/search?q=diagnosis", asTenant("acme")).DataList() {
		if m, _ := h.(map[string]any); m["model"] == "publicMemo" {
			t.Errorf("publicMemo has no tenant_id column, so a tenant scope must exclude it, "+
				"not pass it through: %v", m)
		}
	}
}

// MUST STILL WORK: with no scope registered at all, search is unconstrained —
// the filters applied are the Forced ones, and there are none.
func TestSearchScope_UnscopedSearchStillReturnsEverything(t *testing.T) {
	t.Parallel()
	s := searchScopeServer(t, nil, func(*maniflex.Server) {})
	seedTwoTenants(t, s)

	if n := len(s.GET("/search?q=diagnosis", asTenant("acme")).DataList()); n != 2 {
		t.Errorf("got %d hit(s), want 2 — an unscoped search must not start filtering", n)
	}
}

// MUST STILL WORK: a client cannot scope a search from the query string.
//
// Structurally it cannot today — /search builds no QueryParams from the URL, so
// ctx.Query is nil unless a scope middleware creates it — and searchScope reads
// only Forced filters, which is what keeps this true if search ever starts
// parsing ?filter=. That second rule is pinned in the root package, where a
// QueryParams can be handed to searchScope directly; this is the end-to-end
// guard.
func TestSearchScope_ClientFilterDoesNotReachTheSearch(t *testing.T) {
	t.Parallel()
	s := searchScopeServer(t, nil, func(*maniflex.Server) {})
	seedTwoTenants(t, s)

	if n := len(s.GET("/search?q=diagnosis&filter=tenant_id:eq:acme", asTenant("acme")).
		DataList()); n != 2 {
		t.Errorf("got %d hit(s), want 2 — a client ?filter= must not scope a search", n)
	}
}

// ── (b) a scope registered anywhere else is named at boot ─────────────────────

// The conventional registration — on the DB step, where db.Tenancy's own example
// puts it — cannot reach /search. It is now reported by name at boot, since
// nothing can report it at runtime: the request simply arrives unscoped and
// answers 200.
func TestSearchScope_BootWarnsWhenTheScopeCannotReachSearch(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	s := searchScopeServer(t, &logs, func(srv *maniflex.Server) {
		srv.Pipeline.DB.Register(tenantScope())
	})
	s.GET("/search?q=diagnosis", asTenant("acme")).AssertStatus(http.StatusOK)

	out := logs.String()
	for _, want := range []string{
		"not row-scoped", "scopedMemo", "db.Tenancy", "DB", "does not run the DB step",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("boot output does not mention %q:\n%s", want, out)
		}
	}
}

// ForModel is the second reason, and a different remedy, so it gets its own
// message: the registration is on a step search runs, but search runs on the
// synthetic model that ForModel can never name.
func TestSearchScope_BootWarnsWhenTheScopeIsModelRestricted(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	searchScopeServer(t, &logs, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(tenantScope(),
			maniflex.ProvidesScope(), maniflex.ForModel("scopedMemo"))
	}).GET("/search?q=x", asTenant("acme"))

	out := logs.String()
	for _, want := range []string{"scopedMemo", "ForModel", "__search"} {
		if !strings.Contains(out, want) {
			t.Errorf("boot output does not mention %q:\n%s", want, out)
		}
	}
}

// auth.Enforce is marked too. It cannot scope a search at all — a policy needs a
// record and search returns an id and a snippet — but registered where search
// runs it refuses the request, which is a safe answer. On the DB step it does
// neither, so the model is left open and that is what boot must say.
func TestSearchScope_BootWarnsForEnforceOnTheDBStep(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	searchScopeServer(t, &logs, func(srv *maniflex.Server) {
		srv.Pipeline.DB.Register(auth.Enforce(
			func(*maniflex.ServerContext, map[string]any) (bool, error) { return true, nil }))
	}).GET("/search?q=x", asTenant("acme"))

	if out := logs.String(); !strings.Contains(out, "auth.Enforce") {
		t.Errorf("boot output does not name auth.Enforce:\n%s", out)
	}
}

// MUST STILL WORK: the registration that works draws no warning. A check that
// cried wolf on the correct configuration would train operators to ignore it.
func TestSearchScope_NoWarningWhenTheScopeReachesSearch(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	searchScopeServer(t, &logs, func(srv *maniflex.Server) {
		srv.Pipeline.Auth.Register(tenantScope(), maniflex.ProvidesScope())
	}).GET("/search?q=x", asTenant("acme"))

	if out := logs.String(); strings.Contains(out, "not row-scoped") {
		t.Errorf("a scope that does reach /search must not be warned about:\n%s", out)
	}
}

// MUST STILL WORK: a model that is not in the search set is nobody's problem.
// publicMemo is searchable but carries no scope, and scopedMemo here is not
// GlobalSearchable, so there is nothing to report.
func TestSearchScope_NoWarningWhenTheModelIsNotSearchable(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	testutil.NewServer(t, testutil.Options{
		Models: []any{scopedMemo{}, publicMemo{}, maniflex.ModelConfig{GlobalSearchable: true}},
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Middleware: func(s *maniflex.Server) {
			s.EnableGlobalSearch(maniflex.GlobalSearchConfig{MaxLimit: 20})
			s.Pipeline.DB.Register(tenantScope(), maniflex.ForModel("scopedMemo"))
		},
	}).GET("/public_memos")

	if out := logs.String(); strings.Contains(out, "not row-scoped") {
		t.Errorf("scopedMemo is not GlobalSearchable; nothing to warn about:\n%s", out)
	}
}

// MUST STILL WORK: global search off means no report at all, however the app
// scoped its models.
func TestSearchScope_NoWarningWhenGlobalSearchIsOff(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	testutil.NewServer(t, testutil.Options{
		Models: searchScopeModels(),
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(tenantScope())
		},
	}).GET("/scoped_memos", asTenant("acme"))

	if out := logs.String(); strings.Contains(out, "not row-scoped") {
		t.Errorf("EnableGlobalSearch was never called:\n%s", out)
	}
}

// Under Config.Strict the same gap refuses to start, and the message marks
// itself strict-only so an operator knows which knob made it fatal.
func TestSearchScope_StrictRefusesToStart(t *testing.T) {
	t.Parallel()
	srv := maniflex.New(maniflex.Config{Strict: true, Port: 0, DisableAutoMigrate: true})
	if err := srv.Register(scopedMemo{}, maniflex.ModelConfig{GlobalSearchable: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv.EnableGlobalSearch(maniflex.GlobalSearchConfig{MaxLimit: 20})
	srv.Pipeline.DB.Register(tenantScope())

	msg := searchScopeBootPanic(func() { srv.Handler() })
	if msg == "" {
		t.Fatal("Config.Strict must refuse an unscoped global search")
	}
	for _, want := range []string{"scopedMemo", "db.Tenancy", "Config.Strict"} {
		if !strings.Contains(msg, want) {
			t.Errorf("strict failure missing %q:\n%s", want, msg)
		}
	}
}

// searchScopeBootPanic returns the message fn panicked with, or "" if it did
// not. strict_startup_test.go has the same helper, but it lives in the internal
// e2e package rather than this external one.
func searchScopeBootPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	fn()
	return ""
}
