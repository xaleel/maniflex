package e2e

// P1-18 — a middleware on the Auth step could not contribute a query filter: the
// Deserialize step builds ctx.Query from the request and used to assign it
// wholesale, discarding anything an earlier step had appended. jobs/maniflex's
// own per-actor scope did exactly that, so job statuses were never scoped and any
// authenticated non-admin listed every actor's (and every tenant's) rows.
//
// The fix: Deserialize now carries over filters marked FilterExpr.Forced. A scope
// the server imposes survives the rebuild; a plain filter (and anything derived
// from the request) still comes from the parse alone.

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/jobs"
	"github.com/xaleel/maniflex/jobs/inproc"
	jobsmaniflex "github.com/xaleel/maniflex/jobs/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── the core mechanism: Forced survives, plain does not ─────────────────────────

type Memo struct {
	maniflex.BaseModel
	Owner string `json:"owner" db:"owner" mfx:"filterable"`
	Text  string `json:"text" db:"text"`
}

// ownerScope appends an owner filter on the Auth step (before Deserialize), forced
// or not, when the request carries an X-Owner header.
func ownerScope(forced bool) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		owner := ctx.Request.Header.Get("X-Owner")
		if owner == "" {
			return next()
		}
		if ctx.Query == nil {
			ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
		}
		ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
			Field: "owner", Operator: maniflex.OpEq, Value: owner, Forced: forced,
		})
		return next()
	}
}

func memoServer(t *testing.T, forced bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{Memo{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(ownerScope(forced))
		},
	})
}

func total(t *testing.T, r *testutil.Response) float64 {
	t.Helper()
	var got float64
	r.AssertStatus(http.StatusOK).AssertJSON(func(body map[string]any) {
		meta, _ := body["meta"].(map[string]any)
		got, _ = meta["total"].(float64)
	})
	return got
}

func TestForcedFilter_SetOnAuthStepSurvivesDeserialize(t *testing.T) {
	srv := memoServer(t, true)
	srv.POST("/memos", map[string]any{"owner": "alice", "text": "a"}).AssertStatus(http.StatusCreated)
	srv.POST("/memos", map[string]any{"owner": "bob", "text": "b"}).AssertStatus(http.StatusCreated)

	// The Auth-step forced filter reaches the DB step, so alice sees only her row.
	if got := total(t, srv.GET("/memos", map[string]string{"X-Owner": "alice"})); got != 1 {
		t.Errorf("forced scope: got total %v, want 1 (alice's own)", got)
	}

	// A client cannot escape the forced scope by adding its own filter.
	if got := total(t, srv.GET("/memos?filter=owner:eq:bob", map[string]string{"X-Owner": "alice"})); got != 0 {
		t.Errorf("forced scope escaped via ?filter=: got total %v, want 0", got)
	}
}

func TestForcedFilter_PlainAuthFilterIsStillDiscarded(t *testing.T) {
	srv := memoServer(t, false)
	srv.POST("/memos", map[string]any{"owner": "alice", "text": "a"}).AssertStatus(http.StatusCreated)
	srv.POST("/memos", map[string]any{"owner": "bob", "text": "b"}).AssertStatus(http.StatusCreated)

	// A non-Forced filter set before Deserialize is not preserved — only the Forced
	// flag opts a scope into surviving the rebuild, so this still sees both rows.
	if got := total(t, srv.GET("/memos", map[string]string{"X-Owner": "alice"})); got != 2 {
		t.Errorf("plain (non-forced) Auth filter should be discarded: got total %v, want 2", got)
	}
}

// ── the shipped leak: jobs/maniflex job statuses ────────────────────────────────

func actorAuthMW(ctx *maniflex.ServerContext, next func() error) error {
	uid := ctx.Request.Header.Get("X-Actor")
	if uid == "" {
		return next()
	}
	ai := &maniflex.AuthInfo{UserID: uid}
	if role := ctx.Request.Header.Get("X-Role"); role != "" {
		ai.Roles = []string{role}
	}
	if tn := ctx.Request.Header.Get("X-Tenant"); tn != "" {
		ai.TenantID = tn
	}
	ctx.Auth = ai
	return next()
}

func asActor(id string) map[string]string { return map[string]string{"X-Actor": id} }
func asJobsAdmin() map[string]string {
	return map[string]string{"X-Actor": "admin-u", "X-Role": "admin"}
}

func newJobsServerAuth(t *testing.T) (*testutil.Server, jobs.Queue) {
	t.Helper()
	raw := inproc.New()
	var q jobs.Queue
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			// Registered before Mount, so it runs before the per-actor scope and
			// populates ctx.Auth that the scope reads.
			s.Pipeline.Auth.Register(actorAuthMW)
			_, wrapped, err := jobsmaniflex.Mount(s, raw)
			if err != nil {
				t.Fatalf("jobsmaniflex.Mount: %v", err)
			}
			q = wrapped
		},
	})
	return srv, q
}

func TestJobs_ForceFilterNowScopesToActor(t *testing.T) {
	srv, q := newJobsServerAuth(t)
	ctx := context.Background()
	id1, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-1"})
	if err != nil {
		t.Fatalf("Enqueue actor-1: %v", err)
	}
	id2, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-2"})
	if err != nil {
		t.Fatalf("Enqueue actor-2: %v", err)
	}

	// The bug: actor-1 used to list every actor's rows. Now it lists only its own.
	if got := total(t, srv.GET("/job_statuses", asActor("actor-1"))); got != 1 {
		t.Errorf("actor-1 list: got total %v, want 1 (its own row)", got)
	}

	// No existence oracle: actor-1 cannot read actor-2's row by id.
	srv.GET("/job_statuses/"+id2, asActor("actor-1")).AssertStatus(http.StatusNotFound)
	// but reads its own.
	srv.GET("/job_statuses/"+id1, asActor("actor-1")).AssertStatus(http.StatusOK)

	// admin bypasses the scope and sees both.
	if got := total(t, srv.GET("/job_statuses", asJobsAdmin())); got != 2 {
		t.Errorf("admin list: got total %v, want 2", got)
	}

	// actor-1 cannot escape scope by filtering for actor-2.
	if got := total(t, srv.GET("/job_statuses?filter=actor_id:eq:actor-2", asActor("actor-1"))); got != 0 {
		t.Errorf("actor-1 escaped its scope via ?filter=: got total %v, want 0", got)
	}
}

func TestJobs_ForceFilterScopesToTenant(t *testing.T) {
	srv, q := newJobsServerAuth(t)
	ctx := context.Background()
	idA, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-1", TenantID: "tenant-A"})
	if err != nil {
		t.Fatalf("Enqueue tenant-A: %v", err)
	}
	idB, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-1", TenantID: "tenant-B"})
	if err != nil {
		t.Fatalf("Enqueue tenant-B: %v", err)
	}

	// Same actor, two tenants: the caller in tenant-A sees only the tenant-A row.
	hdr := map[string]string{"X-Actor": "actor-1", "X-Tenant": "tenant-A"}
	if got := total(t, srv.GET("/job_statuses", hdr)); got != 1 {
		t.Errorf("tenant-A list: got total %v, want 1", got)
	}
	srv.GET("/job_statuses/"+idB, hdr).AssertStatus(http.StatusNotFound)
	srv.GET("/job_statuses/"+idA, hdr).AssertStatus(http.StatusOK)
}
