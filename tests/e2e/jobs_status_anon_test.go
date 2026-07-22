package e2e

// Audit JB-12: jobs/maniflex's per-actor scope guard was
//
//	if ctx.HasRole(adminRole) || ctx.Auth == nil { return next() }
//
// so a request with no authenticated caller skipped the scope entirely and the
// list came back unfiltered — every actor's and every tenant's job metadata
// (type, status, timings, actor_id, tenant_id). HasRole is nil-safe, so the
// second clause was the whole of it: wherever /job_statuses was reachable without
// auth, it was an unscoped read of the whole table.
//
// It now fails closed: no identity on a per-actor resource is a 401.
//
//	go test ./e2e/ -run TestJobsStatusAnon

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex/jobs"
)

// seedTwoActors enqueues one job for each of two different actors/tenants, so an
// unscoped read is visibly distinguishable from a scoped one.
func seedTwoActors(t *testing.T, q jobs.Queue) (string, string) {
	t.Helper()
	ctx := context.Background()
	id1, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-1", TenantID: "tenant-A"})
	if err != nil {
		t.Fatalf("enqueue actor-1: %v", err)
	}
	id2, err := q.Enqueue(ctx, jobs.Job{Type: "x", ActorID: "actor-2", TenantID: "tenant-B"})
	if err != nil {
		t.Fatalf("enqueue actor-2: %v", err)
	}
	return id1, id2
}

// The leak: an anonymous list used to answer 200 with both actors' rows.
func TestJobsStatusAnon_ListIsRefused(t *testing.T) {
	srv, q := newJobsServerAuth(t)
	seedTwoActors(t, q)

	// No X-Actor header, so actorAuthMW leaves ctx.Auth nil.
	srv.GET("/job_statuses").AssertStatus(http.StatusUnauthorized)
}

// The same by id — an anonymous read must not be an existence oracle either.
func TestJobsStatusAnon_ReadIsRefused(t *testing.T) {
	srv, q := newJobsServerAuth(t)
	id1, _ := seedTwoActors(t, q)

	srv.GET("/job_statuses/" + id1).AssertStatus(http.StatusUnauthorized)
}

// Anti-over-reach: failing closed for anonymous callers must not disturb the
// authenticated paths — the actor scope and the admin bypass both still work.
func TestJobsStatusAnon_AuthenticatedCallersUnaffected(t *testing.T) {
	srv, q := newJobsServerAuth(t)
	id1, id2 := seedTwoActors(t, q)

	if got := total(t, srv.GET("/job_statuses", asActor("actor-1"))); got != 1 {
		t.Errorf("actor-1 list: got total %v, want 1 (its own row)", got)
	}
	srv.GET("/job_statuses/"+id1, asActor("actor-1")).AssertStatus(http.StatusOK)
	srv.GET("/job_statuses/"+id2, asActor("actor-1")).AssertStatus(http.StatusNotFound)

	if got := total(t, srv.GET("/job_statuses", asJobsAdmin())); got != 2 {
		t.Errorf("admin list: got total %v, want 2", got)
	}
}
