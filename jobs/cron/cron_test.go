package cron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/jobs"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

// recordQueue records what a Scheduler enqueued. Only EnqueueAt is exercised.
type recordQueue struct {
	mu    sync.Mutex
	fired []time.Time
}

func (q *recordQueue) EnqueueAt(_ context.Context, j jobs.Job, at time.Time) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.fired = append(q.fired, at)
	return "id", nil
}

func (q *recordQueue) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	return q.EnqueueAt(ctx, j, time.Now())
}

func (q *recordQueue) EnqueueBatch(context.Context, []jobs.Job) ([]string, error) {
	return nil, nil
}
func (q *recordQueue) Close() error { return nil }

func (q *recordQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.fired)
}

// memLocker is a single-winner-per-key lock shared by several Schedulers, which
// is what a Redis SETNX or a unique INSERT gives you across replicas. The ttl is
// recorded but not enforced: no test outlives one.
type memLocker struct {
	mu    sync.Mutex
	held  map[string]time.Duration
	calls []string
	err   error
}

func newMemLocker() *memLocker {
	return &memLocker{held: map[string]time.Duration{}}
}

func (l *memLocker) Acquire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, key)
	if l.err != nil {
		return false, l.err
	}
	if _, taken := l.held[key]; taken {
		return false, nil
	}
	l.held[key] = ttl
	return true, nil
}

func (l *memLocker) acquiredCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── fireKey ───────────────────────────────────────────────────────────────────

// The whole scheme rests on replicas agreeing on the interval without agreeing
// on when they started. Two Schedulers booted 40 minutes apart tick 40 minutes
// apart; if their keys differed the Locker would never see contention and would
// be decorative.
func TestFireKey_UnalignedTicksShareABucket(t *testing.T) {
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	early := time.Date(2026, 7, 22, 13, 0, 3, 0, time.UTC)
	late := time.Date(2026, 7, 22, 13, 40, 51, 0, time.UTC)

	if a, b := fireKey(e, early), fireKey(e, late); a != b {
		t.Fatalf("ticks in the same hour produced different keys:\n  %s\n  %s", a, b)
	}
}

// A local-zone tick and a UTC tick naming the same instant must agree too —
// Truncate works on the absolute time, so the zone must not leak into the key.
func TestFireKey_ZoneDoesNotChangeTheBucket(t *testing.T) {
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	utc := time.Date(2026, 7, 22, 13, 20, 0, 0, time.UTC)
	elsewhere := utc.In(time.FixedZone("+0530", 5*3600+1800))

	if a, b := fireKey(e, utc), fireKey(e, elsewhere); a != b {
		t.Fatalf("same instant in two zones produced different keys:\n  %s\n  %s", a, b)
	}
}

func TestFireKey_AdjacentIntervalsDiffer(t *testing.T) {
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	this := time.Date(2026, 7, 22, 13, 59, 59, 0, time.UTC)
	next := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)

	if a, b := fireKey(e, this), fireKey(e, next); a == b {
		t.Fatalf("consecutive intervals shared key %s — the schedule would fire once and stop", a)
	}
}

// Every is part of the key, so a shared Type across two different intervals is
// already unambiguous; Name is for the case Every cannot separate.
func TestFireKey_NameSeparatesOtherwiseIdenticalEntries(t *testing.T) {
	at := time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)
	base := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	hourly := fireKey(base, at)
	daily := fireKey(Entry{Every: 24 * time.Hour, Job: jobs.Job{Type: "report"}}, at)
	if hourly == daily {
		t.Fatalf("two intervals of the same type shared key %s", hourly)
	}

	eu := base
	eu.Name = "report-eu"
	us := base
	us.Name = "report-us"
	if a, b := fireKey(eu, at), fireKey(us, at); a == b {
		t.Fatalf("Name did not separate two identical entries: both %s", a)
	}

	// Defaulting to Type keeps every pre-existing entry working unchanged.
	if got, want := fireKey(base, at), fireKey(Entry{Every: time.Hour, Name: "report"}, at); got != want {
		t.Fatalf("empty Name did not default to Job.Type:\n  %s\n  %s", got, want)
	}
}

// ── claim ─────────────────────────────────────────────────────────────────────

// The JB-16 fix proper: three replicas ticking at three different moments inside
// one interval, and exactly one of them gets to enqueue.
func TestClaim_OneReplicaWinsEachInterval(t *testing.T) {
	lock := newMemLocker()
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	replicas := []*Scheduler{
		New(&recordQueue{}, quietLogger(), WithLocker(lock)),
		New(&recordQueue{}, quietLogger(), WithLocker(lock)),
		New(&recordQueue{}, quietLogger(), WithLocker(lock)),
	}
	// Unaligned ticks within the 13:00 interval, then within 14:00.
	ticks := []time.Time{
		time.Date(2026, 7, 22, 13, 0, 3, 0, time.UTC),
		time.Date(2026, 7, 22, 13, 22, 40, 0, time.UTC),
		time.Date(2026, 7, 22, 13, 51, 9, 0, time.UTC),
	}

	for _, hour := range []int{13, 14} {
		won := 0
		for i, r := range replicas {
			if r.claim(context.Background(), e, ticks[i].Add(time.Duration(hour-13)*time.Hour)) {
				won++
			}
		}
		if won != 1 {
			t.Fatalf("hour %d: %d replicas fired, want exactly 1", hour, won)
		}
	}
	if got := lock.acquiredCount(); got != 2 {
		t.Fatalf("held %d keys, want 2 (one per interval)", got)
	}
}

// A replica whose ticker drifts twice into one interval must not fire twice,
// which is the same guarantee viewed from a single process.
func TestClaim_SameReplicaCannotFireAnIntervalTwice(t *testing.T) {
	s := New(&recordQueue{}, quietLogger(), WithLocker(newMemLocker()))
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}
	at := time.Date(2026, 7, 22, 13, 0, 3, 0, time.UTC)

	if !s.claim(context.Background(), e, at) {
		t.Fatal("first claim was refused")
	}
	if s.claim(context.Background(), e, at.Add(30*time.Second)) {
		t.Fatal("second claim in the same interval was granted")
	}
}

// No Locker is the documented default and must keep firing everywhere, or the
// fix would silently change single-process deployments.
func TestClaim_WithoutALockerEveryReplicaFires(t *testing.T) {
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}
	at := time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)

	for i := range 3 {
		s := New(&recordQueue{}, quietLogger())
		if !s.claim(context.Background(), e, at) {
			t.Fatalf("replica %d was refused with no Locker configured", i)
		}
	}
}

// A lock backend outage must not silently stop the schedule.
func TestClaim_LockerErrorFiresAnyway(t *testing.T) {
	lock := newMemLocker()
	lock.err = errors.New("redis: connection refused")
	s := New(&recordQueue{}, quietLogger(), WithLocker(lock))
	e := Entry{Every: time.Hour, Job: jobs.Job{Type: "report"}}

	if !s.claim(context.Background(), e, time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)) {
		t.Fatal("a Locker error suppressed the firing; it must fail open")
	}
}

func TestClaim_TTLCoversTheInterval(t *testing.T) {
	lock := newMemLocker()
	s := New(&recordQueue{}, quietLogger(), WithLocker(lock))
	e := Entry{Every: 24 * time.Hour, Job: jobs.Job{Type: "report"}}

	s.claim(context.Background(), e, time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC))

	lock.mu.Lock()
	defer lock.mu.Unlock()
	if len(lock.held) != 1 {
		t.Fatalf("held %d keys, want 1 — nothing to assert a ttl on", len(lock.held))
	}
	for key, ttl := range lock.held {
		if ttl < e.Every {
			t.Fatalf("key %s held for %s, shorter than the %s interval it guards", key, ttl, e.Every)
		}
	}
}

// ── wiring ────────────────────────────────────────────────────────────────────

// claim is only worth testing if run actually consults it. A Locker that refuses
// everything must produce no enqueues at all.
func TestScheduler_RefusedClaimEnqueuesNothing(t *testing.T) {
	deny := &denyLocker{}
	q := &recordQueue{}
	s := New(q, quietLogger(), WithLocker(deny))
	s.Add(Entry{Every: 5 * time.Millisecond, Job: jobs.Job{Type: "report"}})

	s.Start(context.Background())
	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if deny.count() == 0 {
		t.Fatal("the Locker was never consulted — no tick fired, test proves nothing")
	}
	if got := q.count(); got != 0 {
		t.Fatalf("enqueued %d jobs despite every claim being refused", got)
	}
}

// The same schedule with no Locker still fires, so the test above is measuring
// the Locker and not a dead ticker.
func TestScheduler_WithoutALockerStillFires(t *testing.T) {
	q := &recordQueue{}
	s := New(q, quietLogger())
	s.Add(Entry{Every: 5 * time.Millisecond, Job: jobs.Job{Type: "report"}})

	s.Start(context.Background())
	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if q.count() == 0 {
		t.Fatal("no jobs enqueued without a Locker")
	}
}

// denyLocker refuses every claim and counts how often it was asked.
type denyLocker struct {
	mu sync.Mutex
	n  int
}

func (l *denyLocker) Acquire(context.Context, string, time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return false, nil
}

func (l *denyLocker) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

var (
	_ jobs.Queue = (*recordQueue)(nil)
	_ Locker     = (*memLocker)(nil)
	_ Locker     = (*denyLocker)(nil)
)
