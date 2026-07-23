// Package scheduled applies time-driven state transitions to registered models.
//
// One declarative tag, mfx:"scheduled", on any *time.Time column makes a
// background runner act on the row once that timestamp falls in the past:
// soft-delete it, hard-delete it, or set a sibling field to a fixed value.
//
// A model that declares no scheduled tag pulls in no goroutine and no work —
// New returns a usable no-op Runner so callers can wire it unconditionally.
//
// The package depends only on maniflex; the durable/distributed path is wired by
// the caller with jobs/cron (see scheduled/jobsx). To run Start on several
// replicas without firing hooks N times, pass a Config.Locker for leader
// election, or run Start in exactly one process.
package scheduled

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/xaleel/maniflex"
)

// Locker elects one replica per tick, so a Runner started on N replicas sweeps
// once per interval instead of N times. It is the in-process analogue of the
// jobs/cron Locker and has the same contract.
//
// Acquire must be atomic across replicas and report true to exactly one caller
// per key: Redis SET key val NX PX ttl, an INSERT on a unique column, a Postgres
// advisory lock — whatever the deployment already runs. There is deliberately no
// release: the lock records "this interval has been swept", so it must outlive
// the tick and expire with its ttl; releasing it would let another replica that
// ticks within the same interval sweep again.
type Locker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Config tunes the Runner. All fields are optional.
type Config struct {
	Interval  time.Duration    // tick interval; default 1m
	BatchSize int              // max rows per model per spec per tick; default 500
	Logger    *slog.Logger     // default slog.Default()
	Clock     func() time.Time // injectable; default time.Now (UTC). Tests override.

	// Locker, if set, gates each tick so only one replica sweeps per interval.
	// Without it — the default — every replica running Start sweeps: writes stay
	// idempotent (a second replica's action matches 0 rows and is skipped), but a
	// set-field transition fires OnSetField once per replica. Set a Locker for
	// single-firing hooks across replicas, or run Start in exactly one process.
	Locker Locker

	// Hooks fire once per affected row after the per-model tx commits. Use them
	// to publish events / write an audit trail. They run outside the tx.
	OnDelete   func(model, id string)
	OnSetField func(model, id, field, to string)
}

// Report is the outcome of one Sweep, per model and aggregate.
type Report struct {
	Deleted  int
	Updated  int
	Skipped  int // rows whose action matched 0 rows (already gone) — skipped, not errored
	PerModel map[string]ModelCount
	Errors   []error // non-fatal per-model errors; Sweep continues past them
}

// ModelCount is the per-model tally inside a Report.
type ModelCount struct {
	Deleted int
	Updated int
}

// Runner sweeps every registered model that declares a scheduled tag.
type Runner struct {
	db     maniflex.DBAdapter
	models []*maniflex.ModelMeta
	cfg    Config

	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// hardDeleter is the optional interface a Tx implements to physically delete a
// row even on a soft-deletable model. The sqlcore txAdapter satisfies it.
type hardDeleter interface {
	HardDelete(ctx context.Context, model *maniflex.ModelMeta, id string) error
}

// New inspects server's registry and returns a Runner bound to the models that
// declare scheduled fields. Returns a usable no-op Runner if none do, so
// callers can wire it unconditionally. Errors only on a nil server/DB.
func New(server *maniflex.Server, cfg Config) (*Runner, error) {
	if server == nil {
		return nil, errors.New("scheduled: nil server")
	}
	db := server.DB()
	if db == nil {
		return nil, errors.New("scheduled: server has no database adapter")
	}

	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}

	var models []*maniflex.ModelMeta
	for _, m := range server.Registry().All() {
		if m.HasScheduled() {
			models = append(models, m)
		}
	}

	return &Runner{db: db, models: models, cfg: cfg}, nil
}

// Start launches the tick loop in a background goroutine. Returns immediately.
// One tick runs at t0 before the first interval elapses so a just-booted
// replica catches a backlog without waiting a full Interval.
func (r *Runner) Start(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.loop(loopCtx)
	}()
}

// Stop cancels the loop and waits for the in-flight tick to finish.
func (r *Runner) Stop() {
	r.mu.Lock()
	r.stopped = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Runner) loop(ctx context.Context) {
	r.tick(ctx) // immediate t0 tick

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Runner) tick(ctx context.Context) {
	// Final backstop: a panic anywhere in the tick (e.g. in the injected Clock
	// or Sweep's own bookkeeping) must not unwind the loop goroutine — that
	// would silently stop every scheduled model for the life of the process.
	// Per-model and per-hook panics are already contained further down; this
	// only catches what slips past them, keeping the ticker alive.
	defer func() {
		if p := recover(); p != nil {
			r.cfg.Logger.Error("scheduled: tick panicked; loop preserved",
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()

	if !r.claim(ctx) {
		return // another replica owns this interval
	}

	rep, err := r.Sweep(ctx)
	if err != nil {
		r.cfg.Logger.Error("scheduled: sweep failed", slog.String("error", err.Error()))
		return
	}
	for _, e := range rep.Errors {
		r.cfg.Logger.Error("scheduled: per-model sweep error", slog.String("error", e.Error()))
	}
}

// claim asks the configured Locker whether this replica owns the current tick.
// With no Locker (the default) every replica sweeps. The key is the tick time
// truncated to Interval, so replicas ticking a few seconds apart still agree on
// one owner per interval; the ttl is one Interval — long enough to outlive the
// firing, short enough to free the next interval. A Locker error fails open: a
// lock outage must not silently stop all scheduled work. Only the loop's tick is
// gated; the exported Sweep (jobsx / admin) always runs.
func (r *Runner) claim(ctx context.Context) bool {
	if r.cfg.Locker == nil {
		return true
	}
	key := "scheduled:sweep:" + r.cfg.Clock().Truncate(r.cfg.Interval).UTC().Format(time.RFC3339)
	ok, err := r.cfg.Locker.Acquire(ctx, key, r.cfg.Interval)
	if err != nil {
		r.cfg.Logger.Warn("scheduled: locker error; sweeping anyway (fail-open)",
			slog.String("key", key),
			slog.String("error", err.Error()),
		)
		return true
	}
	return ok
}

// Sweep runs exactly one pass over all scheduled models and returns a report.
// Exported so it can be unit-tested deterministically, called from a
// jobs.Handler (durable/distributed path), or triggered from an admin
// endpoint. Safe for concurrent callers: every action is idempotent.
func (r *Runner) Sweep(ctx context.Context) (Report, error) {
	rep := Report{PerModel: make(map[string]ModelCount)}
	now := r.cfg.Clock()

	for _, meta := range r.models {
		res, err := r.sweepModel(ctx, meta, now)
		if err != nil {
			rep.Errors = append(rep.Errors, err)
			continue
		}
		rep.Deleted += res.deleted
		rep.Updated += res.updated
		rep.Skipped += res.skipped
		if res.deleted != 0 || res.updated != 0 {
			rep.PerModel[meta.Name] = ModelCount{Deleted: res.deleted, Updated: res.updated}
		}
		// Fire hooks AFTER the tx has committed, outside any transaction.
		for _, h := range res.hooks {
			r.fireHook(h)
		}
	}
	return rep, nil
}

func (r *Runner) fireHook(h hookCall) {
	// A user hook is arbitrary code: recover its panic so it can't unwind the
	// sweep and strand this model's remaining hooks (or later models).
	defer func() {
		if p := recover(); p != nil {
			r.cfg.Logger.Error("scheduled: hook panicked; recovered",
				slog.String("model", h.model),
				slog.String("id", h.id),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()

	switch h.kind {
	case hookDelete:
		if r.cfg.OnDelete != nil {
			r.cfg.OnDelete(h.model, h.id)
		}
	case hookSetField:
		if r.cfg.OnSetField != nil {
			r.cfg.OnSetField(h.model, h.id, h.field, h.to)
		}
	}
}
