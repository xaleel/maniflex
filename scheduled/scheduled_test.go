package scheduled_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/scheduled"
)

// discardLogger silences the ERROR lines the recover sites emit, so a test that
// deliberately triggers a panic doesn't spew a stack trace over `go test`.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// panicFindAdapter wraps a real DBAdapter and panics in FindMany for one model,
// standing in for a panic thrown by an adapter, MapToRecord, or a Tx op deep
// inside sweepModel. Every other method is promoted from the embedded adapter.
type panicFindAdapter struct {
	maniflex.DBAdapter
	model string
}

func (p *panicFindAdapter) FindMany(ctx context.Context, model *maniflex.ModelMeta, q *maniflex.QueryParams) ([]any, int64, error) {
	if model.Name == p.model {
		panic("scheduled-test: injected FindMany panic for " + model.Name)
	}
	return p.DBAdapter.FindMany(ctx, model, q)
}

// memLocker grants each key to exactly one caller (single-winner) and records
// how many times it was consulted (attempts) and how many grants it made, so a
// test can prove both replicas actually contended the same interval key.
type memLocker struct {
	mu       sync.Mutex
	held     map[string]struct{}
	attempts int
	grants   int
}

func newMemLocker() *memLocker { return &memLocker{held: map[string]struct{}{}} }

func (l *memLocker) Acquire(_ context.Context, key string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts++
	if _, ok := l.held[key]; ok {
		return false, nil
	}
	l.held[key] = struct{}{}
	l.grants++
	return true, nil
}

func (l *memLocker) counts() (attempts, grants int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attempts, l.grants
}

// errLocker fails every Acquire, to exercise the fail-open path.
type errLocker struct{ n atomic.Int64 }

func (l *errLocker) Acquire(context.Context, string, time.Duration) (bool, error) {
	l.n.Add(1)
	return false, errors.New("scheduled-test: lock backend down")
}

// editOnFindAdapter simulates a concurrent user edit that lands between the
// sweep's Phase-1 due read and its Phase-2 write: the first FindMany that
// actually surfaces a row for the target model runs edit() (through the real
// adapter, its own committed write) before the sweep proceeds to the tx. The
// sweep therefore carries a stale snapshot into Phase 2.
type editOnFindAdapter struct {
	maniflex.DBAdapter
	model string
	once  sync.Once
	edit  func()
}

func (a *editOnFindAdapter) FindMany(ctx context.Context, meta *maniflex.ModelMeta, q *maniflex.QueryParams) ([]any, int64, error) {
	rows, n, err := a.DBAdapter.FindMany(ctx, meta, q)
	if err == nil && meta.Name == a.model && len(rows) > 0 {
		a.once.Do(a.edit)
	}
	return rows, n, err
}

// ── Test models ───────────────────────────────────────────────────────────────

type Article struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Status    string     `json:"status"     db:"status"     mfx:"enum:draft|published|archived,filterable"`
	PublishAt *time.Time `json:"publish_at" db:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`
	ExpiresAt *time.Time `json:"expires_at" db:"expires_at" mfx:"scheduled;soft-delete"`
	PurgeAt   *time.Time `json:"purge_at"   db:"purge_at"   mfx:"scheduled;hard-delete"`
}

type Banner struct {
	maniflex.BaseModel
	Color        string     `json:"color"          db:"color"          mfx:"required,filterable"`
	HolidayStart *time.Time `json:"holiday_start"  db:"holiday_start"  mfx:"scheduled;field=color;to=red"`
	HolidayEnd   *time.Time `json:"holiday_end"    db:"holiday_end"    mfx:"scheduled;field=color;from=red;to=blue"`
}

type Plain struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name"`
}

// Poison declares a hard-delete spec BEFORE a set-field spec, so a row due for
// both is, within one tick, hard-deleted and then handed a doomed set-field on
// the now-missing id — the SCHED-2 poison-batch shape. (meta.Scheduled preserves
// field declaration order, as the Banner chained-transition test relies on.)
type Poison struct {
	maniflex.BaseModel
	Label   string     `json:"label"    db:"label"`
	PurgeAt *time.Time `json:"purge_at" db:"purge_at" mfx:"scheduled;hard-delete"`
	FlipAt  *time.Time `json:"flip_at"  db:"flip_at"  mfx:"scheduled;field=label;to=done"`
}

// ── Test server ───────────────────────────────────────────────────────────────

type testServer struct {
	server  *maniflex.Server
	db      maniflex.DBAdapter
	runner  *scheduled.Runner
	nowFunc func() time.Time
}

func newTestServer(t *testing.T, models []any, batchSize int) *testServer {
	t.Helper()
	now := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return now }

	server := maniflex.New(maniflex.Config{})
	server.MustRegister(models...)

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("auto-migrate: %v", err)
	}

	if batchSize <= 0 {
		batchSize = 500
	}
	runner, err := scheduled.New(server, scheduled.Config{
		Interval:  time.Hour, // never ticks in tests
		BatchSize: batchSize,
		Clock:     nowFunc,
	})
	if err != nil {
		t.Fatalf("scheduled.New: %v", err)
	}

	t.Cleanup(func() { db.Close() })
	return &testServer{server: server, db: db, runner: runner, nowFunc: nowFunc}
}

// past returns a time that is before the test clock's "now".
func past() time.Time { return time.Date(2020, 1, 1, 6, 0, 0, 0, time.UTC) }

// future returns a time that is after the test clock's "now".
func future() time.Time { return time.Date(2020, 1, 1, 18, 0, 0, 0, time.UTC) }

// now returns the exact instant the test clock uses.
func now() time.Time { return time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC) }

// ptr returns a pointer to t.
func ptr(t time.Time) *time.Time { return &t }

// createArticle creates an Article directly through the DB adapter.
func (ts *testServer) createArticle(t *testing.T, status string, publishAt, expiresAt, purgeAt *time.Time) string {
	t.Helper()
	data := map[string]any{"status": status}
	// Seed timestamps in the same fixed-width form the adapter writes a time.Time
	// value in, so the sweep's due-check comparison behaves as it does in production
	// (a raw RFC3339Nano string would misorder at a whole-second boundary — NEW-1).
	if publishAt != nil {
		data["publish_at"] = maniflex.CanonicalTime(*publishAt)
	}
	if expiresAt != nil {
		data["expires_at"] = maniflex.CanonicalTime(*expiresAt)
	}
	if purgeAt != nil {
		data["purge_at"] = maniflex.CanonicalTime(*purgeAt)
	}
	meta := ts.meta(t, "Article")
	row, err := ts.db.Create(context.Background(), meta, data)
	if err != nil {
		t.Fatalf("create article: %v", err)
	}
	return maniflex.RecordToMap(meta, row)["id"].(string)
}

func (ts *testServer) createBanner(t *testing.T, color string, start, end *time.Time) string {
	t.Helper()
	data := map[string]any{"color": color}
	if start != nil {
		data["holiday_start"] = maniflex.CanonicalTime(*start)
	}
	if end != nil {
		data["holiday_end"] = maniflex.CanonicalTime(*end)
	}
	meta := ts.meta(t, "Banner")
	row, err := ts.db.Create(context.Background(), meta, data)
	if err != nil {
		t.Fatalf("create banner: %v", err)
	}
	return maniflex.RecordToMap(meta, row)["id"].(string)
}

func (ts *testServer) createPoison(t *testing.T, label string, purgeAt, flipAt *time.Time) string {
	t.Helper()
	data := map[string]any{"label": label}
	if purgeAt != nil {
		data["purge_at"] = maniflex.CanonicalTime(*purgeAt)
	}
	if flipAt != nil {
		data["flip_at"] = maniflex.CanonicalTime(*flipAt)
	}
	meta := ts.meta(t, "Poison")
	row, err := ts.db.Create(context.Background(), meta, data)
	if err != nil {
		t.Fatalf("create poison: %v", err)
	}
	return maniflex.RecordToMap(meta, row)["id"].(string)
}

func (ts *testServer) meta(t *testing.T, name string) *maniflex.ModelMeta {
	t.Helper()
	m, ok := ts.server.Registry().Get(name)
	if !ok {
		t.Fatalf("model %q not registered", name)
	}
	return m
}

func (ts *testServer) findByID(t *testing.T, name, id string) (map[string]any, error) {
	t.Helper()
	meta := ts.meta(t, name)
	v, err := ts.db.FindByID(context.Background(), meta, id, &maniflex.QueryParams{Page: 1, Limit: 1})
	if err != nil {
		return nil, err
	}
	return maniflex.RecordToMap(meta, v), nil
}

func (ts *testServer) sweep(t *testing.T) scheduled.Report {
	t.Helper()
	rep, err := ts.runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	return rep
}

// ── Soft-delete tests ─────────────────────────────────────────────────────────

func TestSweep_SoftDelete_DueRow(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", nil, ptr(past()), nil)

	rep := ts.sweep(t)

	if rep.Deleted != 1 {
		t.Errorf("Report.Deleted want 1, got %d", rep.Deleted)
	}
	_, err := ts.findByID(t, "Article", id)
	if err != maniflex.ErrNotFound {
		t.Errorf("expected ErrNotFound after soft-delete, got %v", err)
	}
}

func TestSweep_SoftDelete_FutureRow_Untouched(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(future()), nil)

	rep := ts.sweep(t)
	if rep.Deleted != 0 {
		t.Errorf("future row should not be deleted, Report.Deleted=%d", rep.Deleted)
	}
}

func TestSweep_SoftDelete_NullTimestamp_Untouched(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, nil, nil)

	rep := ts.sweep(t)
	if rep.Deleted != 0 {
		t.Errorf("null timestamp row should not be deleted, Report.Deleted=%d", rep.Deleted)
	}
}

func TestSweep_SoftDelete_Idempotent(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)

	ts.sweep(t) // first sweep soft-deletes

	rep := ts.sweep(t) // second sweep: already-deleted rows are excluded
	if rep.Deleted != 0 {
		t.Errorf("second sweep should be a no-op, Report.Deleted=%d", rep.Deleted)
	}
}

// ── Hard-delete tests ─────────────────────────────────────────────────────────

func TestSweep_HardDelete_DueRow(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", nil, nil, ptr(past()))

	rep := ts.sweep(t)
	if rep.Deleted != 1 {
		t.Errorf("Report.Deleted want 1, got %d", rep.Deleted)
	}
	// Row must be physically gone — even querying raw should miss it.
	_, err := ts.findByID(t, "Article", id)
	if err != maniflex.ErrNotFound {
		t.Errorf("expected ErrNotFound after hard-delete, got %v", err)
	}
}

func TestSweep_HardDelete_Idempotent(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, nil, ptr(past()))
	ts.sweep(t)

	rep := ts.sweep(t)
	if rep.Deleted != 0 {
		t.Errorf("second sweep on hard-deleted model should be 0, got %d", rep.Deleted)
	}
}

// ── Set-field with from= tests ────────────────────────────────────────────────

func TestSweep_SetField_WithFrom_Due(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", ptr(past()), nil, nil)

	rep := ts.sweep(t)
	if rep.Updated != 1 {
		t.Errorf("Report.Updated want 1, got %d", rep.Updated)
	}
	row, err := ts.findByID(t, "Article", id)
	if err != nil {
		t.Fatalf("findByID: %v", err)
	}
	if row["status"] != "published" {
		t.Errorf("status want published, got %v", row["status"])
	}
}

func TestSweep_SetField_WithFrom_WrongState_Untouched(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	// Already published — the from=draft guard prevents re-applying.
	id := ts.createArticle(t, "published", ptr(past()), nil, nil)

	rep := ts.sweep(t)
	if rep.Updated != 0 {
		t.Errorf("already-published row should be untouched, got Updated=%d", rep.Updated)
	}
	row, _ := ts.findByID(t, "Article", id)
	if row["status"] != "published" {
		t.Errorf("status should remain published, got %v", row["status"])
	}
}

func TestSweep_SetField_WithFrom_Idempotent(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", ptr(past()), nil, nil)
	ts.sweep(t) // flips to published

	rep := ts.sweep(t) // second sweep: from=draft no longer matches
	if rep.Updated != 0 {
		t.Errorf("second sweep should be no-op, Updated=%d", rep.Updated)
	}
}

// ── Set-field without from= tests ─────────────────────────────────────────────

func TestSweep_SetField_NoFrom_Due(t *testing.T) {
	ts := newTestServer(t, []any{Banner{}}, 0)
	id := ts.createBanner(t, "green", ptr(past()), nil)

	rep := ts.sweep(t)
	if rep.Updated != 1 {
		t.Errorf("Report.Updated want 1, got %d", rep.Updated)
	}
	row, _ := ts.findByID(t, "Banner", id)
	if row["color"] != "red" {
		t.Errorf("color want red, got %v", row["color"])
	}
}

func TestSweep_SetField_NoFrom_Idempotent(t *testing.T) {
	ts := newTestServer(t, []any{Banner{}}, 0)
	ts.createBanner(t, "green", ptr(past()), nil)
	ts.sweep(t) // sets color=red

	rep := ts.sweep(t) // color already == to=red → neq guard skips
	if rep.Updated != 0 {
		t.Errorf("second sweep should be no-op (color already red), Updated=%d", rep.Updated)
	}
}

func TestSweep_SetField_Future_Untouched(t *testing.T) {
	ts := newTestServer(t, []any{Banner{}}, 0)
	ts.createBanner(t, "green", ptr(future()), nil)

	rep := ts.sweep(t)
	if rep.Updated != 0 {
		t.Errorf("future row should not be updated, Updated=%d", rep.Updated)
	}
}

// ── Clock boundary ────────────────────────────────────────────────────────────

func TestSweep_ClockBoundary_ExactlyNow(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	// expires_at == exactly now → <=now must match.
	ts.createArticle(t, "draft", nil, ptr(now()), nil)

	rep := ts.sweep(t)
	if rep.Deleted != 1 {
		t.Errorf("timestamp exactly equal to now should be acted on (<=), Deleted=%d", rep.Deleted)
	}
}

// ── Chained transitions (Banner) ──────────────────────────────────────────────

func TestSweep_ChainedTransitions_BothPast(t *testing.T) {
	ts := newTestServer(t, []any{Banner{}}, 0)
	// Both HolidayStart and HolidayEnd are in the past.
	// After at most two sweeps color must end at blue.
	id := ts.createBanner(t, "green", ptr(past()), ptr(past()))

	ts.sweep(t) // HolidayStart → red (if color≠red); HolidayEnd skips (color was green, not red)
	ts.sweep(t) // HolidayEnd → blue (color is now red)

	row, err := ts.findByID(t, "Banner", id)
	if err != nil {
		t.Fatalf("findByID: %v", err)
	}
	if row["color"] != "blue" {
		t.Errorf("after two sweeps color want blue, got %v", row["color"])
	}
}

func TestSweep_ChainedTransitions_OnlyStartPast(t *testing.T) {
	ts := newTestServer(t, []any{Banner{}}, 0)
	id := ts.createBanner(t, "green", ptr(past()), ptr(future()))

	ts.sweep(t)

	row, _ := ts.findByID(t, "Banner", id)
	if row["color"] != "red" {
		t.Errorf("only HolidayStart past: color want red, got %v", row["color"])
	}
}

// ── BatchSize cap ─────────────────────────────────────────────────────────────

func TestSweep_BatchSize_Cap(t *testing.T) {
	const batch = 5
	ts := newTestServer(t, []any{Article{}}, batch)
	for range batch + 3 {
		ts.createArticle(t, "draft", nil, ptr(past()), nil)
	}

	rep1 := ts.sweep(t)
	if rep1.Deleted != batch {
		t.Errorf("first sweep: want %d deleted, got %d", batch, rep1.Deleted)
	}

	rep2 := ts.sweep(t)
	if rep2.Deleted != 3 {
		t.Errorf("second sweep: want 3 deleted (remainder), got %d", rep2.Deleted)
	}
}

// ── Multi-model ───────────────────────────────────────────────────────────────

func TestSweep_MultiModel(t *testing.T) {
	ts := newTestServer(t, []any{Article{}, Banner{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)
	ts.createBanner(t, "green", ptr(past()), nil)

	rep := ts.sweep(t)
	if rep.Deleted != 1 {
		t.Errorf("Report.Deleted want 1, got %d", rep.Deleted)
	}
	if rep.Updated != 1 {
		t.Errorf("Report.Updated want 1, got %d", rep.Updated)
	}
	if _, ok := rep.PerModel["Article"]; !ok {
		t.Error("PerModel missing Article")
	}
	if _, ok := rep.PerModel["Banner"]; !ok {
		t.Error("PerModel missing Banner")
	}
}

func TestSweep_NonScheduledModelIgnored(t *testing.T) {
	ts := newTestServer(t, []any{Article{}, Plain{}}, 0)
	// Plain has no scheduled tags; sweep should succeed with no error.
	rep := ts.sweep(t)
	if len(rep.Errors) != 0 {
		t.Errorf("unexpected errors: %v", rep.Errors)
	}
}

// ── No scheduled models ───────────────────────────────────────────────────────

func TestNew_NoScheduledModels_NoOp(t *testing.T) {
	ts := newTestServer(t, []any{Plain{}}, 0)
	rep := ts.sweep(t)
	if rep.Deleted != 0 || rep.Updated != 0 {
		t.Errorf("no-op runner should produce zero Report, got %+v", rep)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("unexpected errors: %v", rep.Errors)
	}
}

// ── Hooks ─────────────────────────────────────────────────────────────────────

func TestSweep_Hooks_Delete(t *testing.T) {
	var mu sync.Mutex
	var deleted []string

	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", nil, ptr(past()), nil)

	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     ts.nowFunc, // reuse the same clock
		OnDelete: func(model, rowID string) {
			mu.Lock()
			deleted = append(deleted, model+"/"+rowID)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runner.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "Article/"+id {
		t.Errorf("OnDelete: want [Article/%s], got %v", id, deleted)
	}
}

func TestSweep_Hooks_SetField(t *testing.T) {
	type call struct{ model, id, field, to string }
	var mu sync.Mutex
	var calls []call

	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", ptr(past()), nil, nil)

	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     ts.nowFunc,
		OnSetField: func(model, rowID, field, to string) {
			mu.Lock()
			calls = append(calls, call{model, rowID, field, to})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("want 1 OnSetField call, got %d", len(calls))
	}
	c := calls[0]
	if c.model != "Article" || c.id != id || c.field != "status" || c.to != "published" {
		t.Errorf("OnSetField call mismatch: %+v", c)
	}
}

func TestSweep_Hooks_FiredAfterCommit(t *testing.T) {
	// The hook must observe the already-committed state.
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", nil, ptr(past()), nil)

	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval:  time.Hour,
		BatchSize: 500,
		Clock:     ts.nowFunc,
		OnDelete: func(model, rowID string) {
			// The row should already be gone from the DB at hook time.
			_, err := ts.db.FindByID(context.Background(), ts.meta(t, "Article"), rowID, &maniflex.QueryParams{Page: 1, Limit: 1})
			if err != maniflex.ErrNotFound {
				t.Errorf("hook fired before commit: FindByID = %v (want ErrNotFound)", err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = id
}

// ── Lifecycle ─────────────────────────────────────────────────────────────────

func TestRunner_Start_ImmediateTick(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)

	var deleted atomic.Int64
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval:  time.Hour, // don't fire again
		BatchSize: 500,
		Clock:     ts.nowFunc,
		OnDelete:  func(_, _ string) { deleted.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.Start(ctx)

	// The immediate t0 tick should fire almost instantly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if deleted.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	runner.Stop()

	if deleted.Load() != 1 {
		t.Errorf("immediate tick: want 1 deleted, got %d", deleted.Load())
	}
}

func TestRunner_Stop_Clean(t *testing.T) {
	ts := newTestServer(t, []any{Plain{}}, 0) // no-op runner
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: time.Hour,
		Clock:    ts.nowFunc,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.Start(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() did not return within 3s — goroutine leak?")
	}
}

func TestRunner_ContextCancel_StopsLoop(t *testing.T) {
	ts := newTestServer(t, []any{Plain{}}, 0)
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: time.Hour,
		Clock:    ts.nowFunc,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		runner.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not stop after ctx cancel within 3s")
	}
}

// ── Concurrent Sweep ──────────────────────────────────────────────────────────

func TestSweep_Concurrent_NoDoubleAction(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	const rows = 10
	for range rows {
		ts.createArticle(t, "draft", nil, ptr(past()), nil)
	}

	var errCount atomic.Int64
	const goroutines = 5
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ts.runner.Sweep(context.Background())
			if err != nil {
				t.Errorf("Sweep error: %v", err)
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	if errCount.Load() != 0 {
		t.Fatalf("concurrent sweeps produced errors")
	}
	// Correctness check: after all concurrent sweeps every row must be gone.
	// (Concurrent soft-deletes are idempotent writes; the important thing is
	// no errors and all rows end up deleted, not that each was acted on exactly once.)
	meta := ts.meta(t, "Article")
	remaining, _, err := ts.db.FindMany(context.Background(), meta, &maniflex.QueryParams{Page: 1, Limit: 100})
	if err != nil {
		t.Fatalf("FindMany: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("want 0 active rows after concurrent sweeps, got %d", len(remaining))
	}
}

// ── ErrNotFound is a per-row skip, not a batch abort (SCHED-2) ──────────────

// A row due for both a hard-delete and a set-field is deleted, then the doomed
// set-field on the now-missing id is skipped — it must not roll back the batch
// and starve the model's other due rows.
func TestSweep_SameRowMultiSpec_DoesNotPoisonBatch(t *testing.T) {
	ts := newTestServer(t, []any{Poison{}}, 0)
	// The poison row: due for hard-delete (purge_at) AND set-field (flip_at).
	poisonID := ts.createPoison(t, "todo", ptr(past()), ptr(past()))
	// An independent row due only for the set-field — it must still drain.
	otherID := ts.createPoison(t, "todo", nil, ptr(past()))

	rep := ts.sweep(t) // ts.sweep fails on a top-level error; a skip is not one

	if _, err := ts.findByID(t, "Poison", poisonID); err != maniflex.ErrNotFound {
		t.Errorf("poison row should be hard-deleted, findByID err = %v (want ErrNotFound)", err)
	}
	other, err := ts.findByID(t, "Poison", otherID)
	if err != nil {
		t.Fatalf("independent row missing: %v", err)
	}
	if other["label"] != "done" {
		t.Errorf("independent row starved by the poison batch: label = %v, want done", other["label"])
	}
	if rep.Deleted != 1 {
		t.Errorf("Report.Deleted want 1 (the poison row), got %d", rep.Deleted)
	}
	if rep.Updated != 1 {
		t.Errorf("Report.Updated want 1 (the independent row), got %d", rep.Updated)
	}
	if rep.Skipped != 1 {
		t.Errorf("Report.Skipped want 1 (the doomed set-field on the deleted row), got %d", rep.Skipped)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("a skipped row must not surface as an error: %v", rep.Errors)
	}
}

// The poison scenario must actually drain: a second sweep is a clean no-op, not
// a re-poisoned batch that re-reads and re-fails the identical rows forever.
func TestSweep_PoisonBatch_DoesNotRecur(t *testing.T) {
	ts := newTestServer(t, []any{Poison{}}, 0)
	ts.createPoison(t, "todo", ptr(past()), ptr(past()))
	otherID := ts.createPoison(t, "todo", nil, ptr(past()))

	ts.sweep(t) // first sweep drains both rows

	rep := ts.sweep(t) // second sweep: nothing left to do
	if rep.Deleted != 0 || rep.Updated != 0 || rep.Skipped != 0 {
		t.Errorf("second sweep should be a clean no-op, got %+v", rep)
	}
	if len(rep.Errors) != 0 {
		t.Errorf("second sweep produced errors: %v", rep.Errors)
	}
	if row, _ := ts.findByID(t, "Poison", otherID); row["label"] != "done" {
		t.Errorf("independent row should stay done, got %v", row["label"])
	}
}

// ── Write-time re-validation closes the TOCTOU (SCHED-4) ────────────────────

// A user moves the guard field off `from` after the sweep's Phase-1 read; the
// Phase-2 write must re-assert the guard under the row lock and skip, not force
// the transition over the user's edit.
func TestSweep_SetField_ConcurrentGuardEdit_NotClobbered(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", ptr(past()), nil, nil) // due for set-field draft→published

	adapter := &editOnFindAdapter{DBAdapter: ts.db, model: "Article", edit: func() {
		meta := ts.meta(t, "Article")
		rec, _ := maniflex.MapToRecord(meta, map[string]any{"status": "archived"})
		if _, err := ts.db.Update(context.Background(), meta, id, rec, map[string]struct{}{"status": {}}); err != nil {
			t.Errorf("concurrent edit failed: %v", err)
		}
	}}
	ts.server.SetDB(adapter)
	runner, err := scheduled.New(ts.server, scheduled.Config{Interval: time.Hour, Clock: ts.nowFunc, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	row, err := ts.findByID(t, "Article", id)
	if err != nil {
		t.Fatalf("findByID: %v", err)
	}
	if row["status"] != "archived" {
		t.Errorf("concurrent edit clobbered: status = %v, want archived (the user's edit)", row["status"])
	}
	if rep.Updated != 0 {
		t.Errorf("the row was no longer due at write time; want Updated 0, got %d", rep.Updated)
	}
	if rep.Skipped != 1 {
		t.Errorf("want Skipped 1 (re-validated as not due), got %d", rep.Skipped)
	}
}

// A user un-schedules a soft-delete (pushes its timestamp into the future) after
// Phase 1; the Phase-2 write must re-check the timestamp under the lock and skip,
// not delete the row anyway.
func TestSweep_SoftDelete_ConcurrentUnschedule_NotDeleted(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	id := ts.createArticle(t, "draft", nil, ptr(past()), nil) // due for soft-delete via expires_at

	adapter := &editOnFindAdapter{DBAdapter: ts.db, model: "Article", edit: func() {
		meta := ts.meta(t, "Article")
		rec, _ := maniflex.MapToRecord(meta, map[string]any{"expires_at": maniflex.CanonicalTime(future())})
		if _, err := ts.db.Update(context.Background(), meta, id, rec, map[string]struct{}{"expires_at": {}}); err != nil {
			t.Errorf("concurrent un-schedule failed: %v", err)
		}
	}}
	ts.server.SetDB(adapter)
	runner, err := scheduled.New(ts.server, scheduled.Config{Interval: time.Hour, Clock: ts.nowFunc, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if _, err := ts.findByID(t, "Article", id); err != nil {
		t.Errorf("the un-scheduled row was deleted anyway: findByID err = %v (want nil)", err)
	}
	if rep.Deleted != 0 {
		t.Errorf("want Deleted 0, got %d", rep.Deleted)
	}
	if rep.Skipped != 1 {
		t.Errorf("want Skipped 1 (re-validated as not due), got %d", rep.Skipped)
	}
}

// ── Leader election via Locker (SCHED-3) ────────────────────────────────────

// With a shared Locker, two replicas ticking the same interval elect one sweeper
// between them, so a set-field transition fires OnSetField once, not once per
// replica.
func TestRunner_WithLocker_OneReplicaSweepsPerInterval(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", ptr(past()), nil, nil) // due for set-field draft→published

	lock := newMemLocker()
	var fired atomic.Int64
	mkRunner := func() *scheduled.Runner {
		r, err := scheduled.New(ts.server, scheduled.Config{
			Interval:   10 * time.Second, // only the immediate t0 tick fires within the test
			Clock:      ts.nowFunc,       // both replicas share an instant → the same interval key
			Locker:     lock,
			Logger:     discardLogger(),
			OnSetField: func(_, _, _, _ string) { fired.Add(1) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := mkRunner(), mkRunner()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.Start(ctx)
	b.Start(ctx)
	defer a.Stop()
	defer b.Stop()

	// Wait until both replicas have contended the lock on their t0 tick.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if att, _ := lock.counts(); att >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	att, grants := lock.counts()
	if att < 2 {
		t.Fatalf("both replicas should have contended the lock, got %d attempts", att)
	}
	if grants != 1 {
		t.Errorf("exactly one replica should win the interval, got %d grants", grants)
	}

	// The winner sweeps once; the loser skipped. OnSetField must fire exactly once.
	fdeadline := time.Now().Add(time.Second)
	for time.Now().Before(fdeadline) {
		if fired.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := fired.Load(); got != 1 {
		t.Errorf("OnSetField should fire once across both replicas, got %d", got)
	}
}

// A Locker outage must not silently stop all scheduled work: claim fails open and
// the tick sweeps anyway.
func TestRunner_LockerError_FailsOpen(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil) // due for soft-delete

	lock := &errLocker{}
	var deleted atomic.Int64
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: 10 * time.Second,
		Clock:    ts.nowFunc,
		Locker:   lock,
		Logger:   discardLogger(),
		OnDelete: func(_, _ string) { deleted.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.Start(ctx)
	defer runner.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if deleted.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if deleted.Load() != 1 {
		t.Fatalf("a Locker error must fail open and sweep: want 1 delete, got %d", deleted.Load())
	}
	if lock.n.Load() == 0 {
		t.Error("the Locker was never consulted — the test proves nothing")
	}
}

// ── Panic recovery (SCHED-1) ────────────────────────────────────────────────

// A panicking user hook must not unwind the sweep: it stays contained to that
// one hook call, so the row's tx still commits and the model's remaining hooks
// still fire. (fireHook recover.)
func TestSweep_PanickingHook_DoesNotStrandOtherRows(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)

	var calls atomic.Int64
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: time.Hour,
		Clock:    ts.nowFunc,
		Logger:   discardLogger(),
		OnDelete: func(_, _ string) {
			calls.Add(1)
			panic("scheduled-test: hook boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without the recover the first hook's panic unwinds Sweep entirely — this
	// call would panic and crash the test goroutine.
	rep, err := runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep returned an error: %v", err)
	}
	if rep.Deleted != 2 {
		t.Errorf("both rows committed before hooks: want Deleted 2, got %d", rep.Deleted)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("both hooks must fire despite the first panicking: want 2 calls, got %d", got)
	}
}

// A panic deep inside one model's sweep must be contained to that model: Sweep
// records it as a per-model error and still sweeps the other models. (sweepModel
// recover.)
func TestSweep_PanickingModel_IsolatedAndReported(t *testing.T) {
	ts := newTestServer(t, []any{Article{}, Banner{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil) // Article: will panic in FindMany
	bannerID := ts.createBanner(t, "green", ptr(past()), nil)

	// Swap the runner's adapter for one that panics on Article's FindMany.
	ts.server.SetDB(&panicFindAdapter{DBAdapter: ts.db, model: "Article"})
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: time.Hour,
		Clock:    ts.nowFunc,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Without the recover Article's panic unwinds Sweep and this call crashes.
	rep, err := runner.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep returned a top-level error: %v", err)
	}
	if len(rep.Errors) != 1 {
		t.Fatalf("want exactly one per-model error for the panicking model, got %d: %v", len(rep.Errors), rep.Errors)
	}
	// The other model was still swept to completion.
	if rep.Updated != 1 {
		t.Errorf("the non-panicking model must still be swept: want Updated 1, got %d", rep.Updated)
	}
	row, _ := ts.findByID(t, "Banner", bannerID)
	if row["color"] != "red" {
		t.Errorf("Banner should have been updated despite Article panicking: color = %v", row["color"])
	}
}

// A panic in the tick itself (here via the injected Clock, above sweepModel and
// fireHook) must not kill the loop goroutine: a later tick still sweeps due
// rows. (tick backstop.)
func TestRunner_Loop_SurvivesPanickingTick(t *testing.T) {
	ts := newTestServer(t, []any{Article{}}, 0)
	ts.createArticle(t, "draft", nil, ptr(past()), nil)

	var clockCalls atomic.Int64
	fixed := now()
	clock := func() time.Time {
		if clockCalls.Add(1) == 1 {
			panic("scheduled-test: clock boom on the first tick")
		}
		return fixed
	}

	var deleted atomic.Int64
	runner, err := scheduled.New(ts.server, scheduled.Config{
		Interval: 40 * time.Millisecond,
		Clock:    clock,
		Logger:   discardLogger(),
		OnDelete: func(_, _ string) { deleted.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.Start(ctx)
	defer runner.Stop()

	// The t0 tick panics in Clock; a subsequent tick (~40ms later) must succeed.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if deleted.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if deleted.Load() < 1 {
		t.Fatal("loop died after a panicking tick: the due row was never swept")
	}
}
