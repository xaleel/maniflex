package e2e

// R6 — middleware/workflow OnTransition hooks.

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/workflow"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// errStoreCredit stands in for a side effect that fails at runtime.
var errStoreCredit = errors.New("store credit unavailable")

// hookLog records what fired, in order, under a mutex — the concurrency tests
// below read it from the test goroutine after their writers have joined.
type hookLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *hookLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, s)
}

func (l *hookLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func (l *hookLog) count(s string) int {
	n := 0
	for _, v := range l.all() {
		if v == s {
			n++
		}
	}
	return n
}

// hooksServer wires WorkflowDoc with a hook-bearing machine on the DB step and
// WithTransaction on Service, which is the documented setup.
func hooksServer(t *testing.T, log *hookLog, opts ...workflow.Option) *testutil.Server {
	t.Helper()
	base := []workflow.Option{
		workflow.Allow("draft", "submitted"),
		workflow.Allow("submitted", "approved", workflow.RequireRole("manager")),
		workflow.Allow("submitted", "rejected", workflow.RequireRole("manager")),
		workflow.AllowInitial("draft", "submitted"),
	}
	sm := workflow.New("status", append(base, opts...)...)

	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.WorkflowDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if raw := ctx.Request.Header.Get("X-Test-Roles"); raw != "" {
					ctx.Auth = &maniflex.AuthInfo{Roles: splitCSV(raw)}
				}
				return next()
			})
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForModel("WorkflowDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
			)
			s.Pipeline.DB.Register(
				sm.Hooks(),
				maniflex.ForModel("WorkflowDoc"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
			)
		},
	})
}

// ── firing ──────────────────────────────────────────────────────────────────

func TestWorkflowHooks_FiresOnMatchingTransition(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("draft", "submitted",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("draft->submitted")
			return nil
		}))

	id := post(t, srv, "draft", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)

	if got := log.all(); len(got) != 1 || got[0] != "draft->submitted" {
		t.Fatalf("hook log: got %v, want [draft->submitted]", got)
	}
}

func TestWorkflowHooks_DoesNotFireOnNonMatchingTransition(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("submitted", "approved",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	id := post(t, srv, "draft", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)

	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times on draft→submitted, want 0", n)
	}
}

// TestWorkflowHooks_FireAllMatching is the divergence from Allow, which is
// first-match-wins. The feature request's own example needs both a specific and
// a wildcard hook to fire for one transition.
func TestWorkflowHooks_FireAllMatching(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log,
		workflow.OnTransition("draft", "submitted", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("specific")
			return nil
		}),
		workflow.OnTransition("*", "submitted", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("wildcard-from")
			return nil
		}),
		workflow.OnTransition("draft", "*", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("wildcard-to")
			return nil
		}),
		workflow.OnTransition("submitted", "approved", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("unrelated")
			return nil
		}),
	)

	id := post(t, srv, "draft", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)

	want := []string{"specific", "wildcard-from", "wildcard-to"}
	got := log.all()
	if len(got) != len(want) {
		t.Fatalf("hook log: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hook log: got %v, want %v (declaration order)", got, want)
		}
	}
}

func TestWorkflowHooks_ReceivesFromAndTo(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add(from + "|" + to)
			return nil
		}))

	id := post(t, srv, "draft", "")
	patch(srv, id, "submitted", "").AssertStatus(http.StatusOK)

	if got := log.all(); len(got) != 1 || got[0] != "draft|submitted" {
		t.Fatalf("hook args: got %v, want [draft|submitted]", got)
	}
}

func TestWorkflowHooks_SeesTheWriteAndTheAuth(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("submitted", "approved",
		func(ctx *maniflex.ServerContext, from, to string) error {
			// The hook runs after the write lands, so a read through the tx
			// observes the new state — that is what "atomic with it" buys.
			rec, err := ctx.GetModel("WorkflowDoc").Read(ctx.ResourceID)
			if err != nil {
				return err
			}
			log.add("stored=" + rec["status"].(string))
			if ctx.HasRole("manager") {
				log.add("actor=manager")
			}
			return nil
		}))

	id := post(t, srv, "submitted", "")
	patch(srv, id, "approved", "manager").AssertStatus(http.StatusOK)

	got := log.all()
	if len(got) != 2 || got[0] != "stored=approved" || got[1] != "actor=manager" {
		t.Fatalf("hook context: got %v, want [stored=approved actor=manager]", got)
	}
}

// ── no-ops ──────────────────────────────────────────────────────────────────

func TestWorkflowHooks_SameStateWriteIsNotATransition(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	id := post(t, srv, "draft", "")
	patch(srv, id, "draft", "").AssertStatus(http.StatusOK)

	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times on a same-state write, want 0", n)
	}
}

func TestWorkflowHooks_PatchNotTouchingStatusIsNoOp(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	id := post(t, srv, "draft", "")
	srv.PATCH("/workflow_docs/"+id, map[string]any{"title": "renamed"}, nil).
		AssertStatus(http.StatusOK)

	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times on a PATCH that never touched status, want 0", n)
	}
}

// A Create seeds an initial state; AllowInitial governs it. It is not a
// transition and must not fire hooks.
func TestWorkflowHooks_DoNotFireOnCreate(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	post(t, srv, "draft", "")

	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times on create, want 0", n)
	}
}

func TestWorkflowHooks_MissingRecordIs404NotAHook(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	patch(srv, "00000000-0000-0000-0000-000000000000", "submitted", "").
		AssertStatus(http.StatusNotFound)

	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times for a missing record, want 0", n)
	}
}

// ── rollback ────────────────────────────────────────────────────────────────

// The headline guarantee: a hook error rolls the transition back.
func TestWorkflowHooks_ErrorRollsTheTransitionBack(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("draft", "submitted",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return errStoreCredit
		}))

	id := post(t, srv, "draft", "")
	resp := patch(srv, id, "submitted", "")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "WORKFLOW_HOOK_ERROR" {
		t.Errorf("error code: got %q, want WORKFLOW_HOOK_ERROR", code)
	}

	// The transition must not have survived the failed hook.
	after := srv.GET("/workflow_docs/"+id, nil)
	after.AssertStatus(http.StatusOK)
	if got := after.Data()["status"]; got != "draft" {
		t.Fatalf("status after a failed hook: got %q, want %q (rollback)", got, "draft")
	}
	if n := log.count("fired"); n != 1 {
		t.Fatalf("hook ran %d times, want 1", n)
	}
}

// A hook may reject with a status of its own by aborting; the write still rolls
// back, but the hook's response is what the client sees.
func TestWorkflowHooks_AbortKeepsTheHooksOwnStatus(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("draft", "submitted",
		func(ctx *maniflex.ServerContext, from, to string) error {
			ctx.Abort(http.StatusConflict, "INSUFFICIENT_CREDIT", "not enough credit")
			return nil
		}))

	id := post(t, srv, "draft", "")
	resp := patch(srv, id, "submitted", "")
	resp.AssertStatus(http.StatusConflict)
	if code := resp.ErrorCode(); code != "INSUFFICIENT_CREDIT" {
		t.Errorf("error code: got %q, want INSUFFICIENT_CREDIT", code)
	}

	after := srv.GET("/workflow_docs/"+id, nil)
	if got := after.Data()["status"]; got != "draft" {
		t.Fatalf("status after an aborting hook: got %q, want %q (rollback)", got, "draft")
	}
}

// ── enforcement: Hooks() is authoritative, not a bare dispatcher ────────────

func TestWorkflowHooks_RejectsUndeclaredTransition(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("*", "*",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	id := post(t, srv, "draft", "")
	resp := patch(srv, id, "approved", "manager") // draft→approved is not declared
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "INVALID_TRANSITION" {
		t.Errorf("error code: got %q, want INVALID_TRANSITION", code)
	}
	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times for a rejected transition, want 0", n)
	}

	after := srv.GET("/workflow_docs/"+id, nil)
	if got := after.Data()["status"]; got != "draft" {
		t.Fatalf("status after a rejected transition: got %q, want draft", got)
	}
}

func TestWorkflowHooks_EnforcesGuards(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("submitted", "approved",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("fired")
			return nil
		}))

	id := post(t, srv, "submitted", "")
	resp := patch(srv, id, "approved", "") // no manager role
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if n := log.count("fired"); n != 0 {
		t.Fatalf("hook fired %d times past a failed guard, want 0", n)
	}

	after := srv.GET("/workflow_docs/"+id, nil)
	if got := after.Data()["status"]; got != "submitted" {
		t.Fatalf("status after a failed guard: got %q, want submitted", got)
	}
}

// ── the TOCTOU the DB-step lock exists to close ─────────────────────────────

// Two concurrent PATCHes both driving pending→confirmed must credit once.
// Without the lock both read from="draft", both pass, and both fire.
//
// Honest scope: on the SQLite lane this test passes even with LockForUpdate
// swapped for a plain in-transaction read (verified). SQLite caps the write pool
// at one connection and opens with _txlock=immediate, so the two transactions
// are already serialised at BEGIN — any in-tx read would do. What the lock buys
// is the Postgres lane, where the two transactions run concurrently under READ
// COMMITTED and a plain read returns "draft" to both; FindByIDForUpdate appends
// FOR UPDATE there (db/sqlcore/adapter.go), so the loser blocks and re-reads the
// committed "submitted". Run with -db=postgres to exercise that.
func TestWorkflowHooks_ConcurrentSameTransitionFiresOnce(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log, workflow.OnTransition("draft", "submitted",
		func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("credit")
			return nil
		}))

	id := post(t, srv, "draft", "")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			patch(srv, id, "submitted", "")
		}()
	}
	close(start)
	wg.Wait()

	if n := log.count("credit"); n != 1 {
		t.Fatalf("credit hook fired %d times for two concurrent draft→submitted PATCHes, want 1 "+
			"(the second must re-read from=submitted under the lock and see a same-state write)", n)
	}
}

// Two concurrent PATCHes racing to *different* targets. Whichever loses the
// lock re-reads a `from` that no longer matches the rule it passed Validate
// with, and must be rejected rather than committing an undeclared transition.
func TestWorkflowHooks_ConcurrentDivergentTransitionsRecheckUnderLock(t *testing.T) {
	t.Parallel()
	log := &hookLog{}
	srv := hooksServer(t, log,
		workflow.OnTransition("submitted", "approved", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("approved")
			return nil
		}),
		workflow.OnTransition("submitted", "rejected", func(ctx *maniflex.ServerContext, from, to string) error {
			log.add("rejected")
			return nil
		}),
	)

	id := post(t, srv, "submitted", "")

	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, 2)
	for i, target := range []string{"approved", "rejected"} {
		wg.Add(1)
		go func(i int, target string) {
			defer wg.Done()
			<-start
			codes[i] = patch(srv, id, target, "manager").Status
		}(i, target)
	}
	close(start)
	wg.Wait()

	// approved→rejected and rejected→approved are both undeclared, so exactly
	// one of the two can succeed.
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("statuses %v: %d succeeded, want exactly 1 — the loser re-reads a "+
			"`from` its rule no longer matches and must be rejected", codes, ok)
	}
	if got := len(log.all()); got != 1 {
		t.Fatalf("hooks fired %v, want exactly one", log.all())
	}
}

// ── wiring errors fail loudly ───────────────────────────────────────────────

func TestWorkflowHooks_MiddlewarePanicsWhenHooksDeclared(t *testing.T) {
	t.Parallel()
	sm := workflow.New("status",
		workflow.Allow("draft", "submitted"),
		workflow.OnTransition("draft", "submitted", func(ctx *maniflex.ServerContext, from, to string) error {
			return nil
		}),
	)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Middleware() with hooks declared must panic — it runs on Validate, " +
				"before the transaction exists, so the hooks would never fire")
		}
		msg, _ := r.(string)
		if !containsSub(msg, "Hooks()") {
			t.Errorf("panic message must name Hooks(); got: %v", r)
		}
	}()
	_ = sm.Middleware()
}

func TestWorkflowHooks_MiddlewareStillWorksWithoutHooks(t *testing.T) {
	t.Parallel()
	sm := workflow.New("status", workflow.Allow("draft", "submitted"))
	if sm.Middleware() == nil {
		t.Fatal("Middleware() must keep working for a machine with no hooks")
	}
}

// Following the lock_scope precedent: no transaction means no lock, and no lock
// means the race is back. Refuse rather than pretend.
func TestWorkflowHooks_NoTransactionIsRefused(t *testing.T) {
	t.Parallel()
	sm := workflow.New("status",
		workflow.Allow("draft", "submitted"),
		workflow.OnTransition("draft", "submitted", func(ctx *maniflex.ServerContext, from, to string) error {
			return nil
		}),
	)
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.WorkflowDoc{}},
		Middleware: func(s *maniflex.Server) {
			// Deliberately no WithTransaction on Service.
			s.Pipeline.DB.Register(
				sm.Hooks(),
				maniflex.ForModel("WorkflowDoc"),
				maniflex.ForOperation(maniflex.OpUpdate),
			)
		},
	})

	id := post(t, srv, "draft", "")
	resp := patch(srv, id, "submitted", "")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "WORKFLOW_NO_TX" {
		t.Errorf("error code: got %q, want WORKFLOW_NO_TX", code)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
