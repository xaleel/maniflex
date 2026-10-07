package e2e

// Audit AUTH-13: auth.ReadAudit wrote its record on a bare goroutine with
// context.Background(), the one fire-and-forget writer in the tree that had not
// moved to ctx.GoBackground.
//
// Three things followed, all executed before the fix:
//
//   - Shutdown returned in 0s without waiting, and the record never arrived. On
//     every rolling deploy the reads served in a replica's last moments were
//     audited nowhere - for a type whose own doc calls this the primary
//     HIPAA-equivalent control surface.
//   - A panic in the sink was on a goroutine no recover reached (chi's Recoverer
//     covers the request goroutine, which has already returned), so it killed
//     the process.
//   - The sink's error was discarded outright.
//
// GoBackground supplies the first two by itself: Server.Shutdown waits for
// tracked goroutines, and backgroundRunner.Go defers recoverBackgroundPanic,
// which reports through Config.PanicLogger and Config.OnBackgroundPanic. The
// fix is to stop hand-rolling the goroutine, not to harden it.
//
//	go test ./tests/e2e/... -run TestReadAuditBackground

import (
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type raBgDoc struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

// raBgSink is a configurable ReadAuditSink that records what it was handed.
type raBgSink struct {
	mu    sync.Mutex
	got   []auth.ReadAuditRecord
	errs  int
	delay time.Duration

	panicOnce bool
	panicked  bool

	// started is closed the first time Write is entered, so a test can wait for
	// the background goroutine to be in flight rather than sleeping.
	startedOnce sync.Once
	started     chan struct{}

	// failWith, when set, is returned by Write.
	failWith error
}

func newRABgSink() *raBgSink {
	return &raBgSink{started: make(chan struct{})}
}

func (s *raBgSink) Write(ctx context.Context, r auth.ReadAuditRecord) error {
	s.startedOnce.Do(func() { close(s.started) })

	s.mu.Lock()
	shouldPanic := s.panicOnce && !s.panicked
	if shouldPanic {
		s.panicked = true
	}
	s.mu.Unlock()
	if shouldPanic {
		panic("read audit sink: deliberate test panic")
	}

	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			s.mu.Lock()
			s.errs++
			s.mu.Unlock()
			return ctx.Err()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
	return s.failWith
}

func (s *raBgSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// lockedLog collects the server's log output so a Warn can be asserted on.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func raBgServer(t *testing.T, sink auth.ReadAuditSink, logs *lockedLog) *testutil.Server {
	t.Helper()
	opts := testutil.Options{
		Models: []any{raBgDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(auth.ReadAudit(sink),
				maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
				maniflex.AtPosition(maniflex.After))
		},
	}
	if logs != nil {
		lg := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		opts.Logger = lg
		opts.PanicLogger = lg
	}
	return testutil.NewServer(t, opts)
}

// THE FINDING. Shutdown waits for a write in flight instead of abandoning it.
// Before the fix Shutdown returned in 0s and the record was never written.
func TestReadAuditBackground_ShutdownWaitsForTheRecord(t *testing.T) {
	sink := newRABgSink()
	sink.delay = 250 * time.Millisecond
	srv := raBgServer(t, sink, nil)

	srv.GET("/ra_bg_docs").AssertStatus(http.StatusOK)

	// The response does not wait for the sink: that is the fire-and-forget
	// property, and it must survive the fix.
	if n := sink.count(); n != 0 {
		t.Fatalf("the HTTP response waited for the sink; records=%d", n)
	}

	// Be sure the write is genuinely in flight before shutting down, so this
	// tests waiting rather than a race we happened to win.
	select {
	case <-sink.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the sink write never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if n := sink.count(); n != 1 {
		t.Errorf("records after shutdown: got %d, want 1 — the record in flight "+
			"was dropped, which is what GoBackground exists to prevent", n)
	}
}

// A panicking sink is recovered and reported rather than killing the process.
// That this test can report anything at all is the assertion: before the fix
// the panic unwound on an untracked goroutine and took the test binary with it.
func TestReadAuditBackground_PanickingSinkDoesNotKillTheProcess(t *testing.T) {
	sink := newRABgSink()
	sink.panicOnce = true
	logs := &lockedLog{}
	srv := raBgServer(t, sink, logs)

	srv.GET("/ra_bg_docs").AssertStatus(http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	got := logs.String()
	if !strings.Contains(got, "recovered from panic in background goroutine") {
		t.Errorf("the panic was not reported through the framework's background "+
			"recovery; logs:\n%s", got)
	}
	// Reported through the framework's own path, which is what reaches
	// Config.OnBackgroundPanic. A recover hand-rolled inside the task would
	// swallow it before that hook could fire.
	if !strings.Contains(got, "deliberate test panic") {
		t.Errorf("the panic value is missing from the report; logs:\n%s", got)
	}
}

// A sink error is logged rather than discarded.
func TestReadAuditBackground_SinkErrorIsLogged(t *testing.T) {
	sink := newRABgSink()
	sink.failWith = context.DeadlineExceeded
	logs := &lockedLog{}
	srv := raBgServer(t, sink, logs)

	srv.GET("/ra_bg_docs").AssertStatus(http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	got := logs.String()
	if !strings.Contains(got, "read audit sink write failed") {
		t.Errorf("a sink error must not be discarded; logs:\n%s", got)
	}
	// Enough context to find the read it belongs to.
	if !strings.Contains(got, "raBgDoc") {
		t.Errorf("the warning does not name the model; logs:\n%s", got)
	}
}

// MUST STILL WORK: the write timeout is honoured, so a sink that blocks does
// not pin a goroutine indefinitely. The sink reports the cancellation itself.
func TestReadAuditBackground_WriteTimeoutStillApplies(t *testing.T) {
	sink := newRABgSink()
	sink.delay = time.Hour // far past the 5s write timeout
	srv := raBgServer(t, sink, nil)

	srv.GET("/ra_bg_docs").AssertStatus(http.StatusOK)
	select {
	case <-sink.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the sink write never started")
	}

	// Shutdown's own deadline cancels the background context, which the sink
	// observes. Either signal arriving is correct; what must not happen is the
	// goroutine outliving the process teardown.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.ManiflexServer().Shutdown(ctx)

	sink.mu.Lock()
	errs := sink.errs
	sink.mu.Unlock()
	if errs == 0 {
		t.Error("a blocked sink was never cancelled; the write context must carry " +
			"both the 5s timeout and the shutdown signal")
	}
}

// MUST STILL WORK: the record itself is still built synchronously and carries
// the request's identity, so moving the write off a bare goroutine did not
// start reading a ServerContext that has moved on.
func TestReadAuditBackground_RecordStillCarriesTheRequestsDetail(t *testing.T) {
	sink := newRABgSink()
	srv := raBgServer(t, sink, nil)

	id := srv.MustID(srv.POST("/ra_bg_docs", map[string]any{"title": "x"}))
	srv.GET("/ra_bg_docs/" + id).AssertStatus(http.StatusOK)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var read *auth.ReadAuditRecord
	for i := range sink.got {
		if sink.got[i].Operation == maniflex.OpRead {
			read = &sink.got[i]
		}
	}
	if read == nil {
		t.Fatalf("no OpRead record was written; got %+v", sink.got)
	}
	if read.RecordID != id {
		t.Errorf("RecordID: got %q, want %q", read.RecordID, id)
	}
	if read.Model != "raBgDoc" {
		t.Errorf("Model: got %q, want raBgDoc", read.Model)
	}
	if read.RequestID == "" {
		t.Error("RequestID is empty; the record must still be built from the request")
	}
}

// One goroutine per audited read, released once its write returns. Documented
// rather than bounded, so it is worth pinning that they do drain: a leak here
// would be invisible until a long-running process ran out of memory.
func TestReadAuditBackground_GoroutinesDrain(t *testing.T) {
	sink := newRABgSink()
	srv := raBgServer(t, sink, nil)

	before := runtime.NumGoroutine()
	for range 40 {
		srv.GET("/ra_bg_docs").AssertStatus(http.StatusOK)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.ManiflexServer().Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if n := sink.count(); n != 40 {
		t.Errorf("records: got %d, want 40 — shutdown must drain every write", n)
	}

	// Shutdown has waited for all of them, so the audit goroutines are gone.
	if after := runtime.NumGoroutine(); after > before+10 {
		t.Errorf("goroutines: %d before, %d after a drained shutdown — the audit "+
			"goroutines did not finish", before, after)
	}
}
