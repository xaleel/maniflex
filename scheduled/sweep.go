package scheduled

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/xaleel/maniflex"
)

// hook kinds collected during a sweep, fired after the per-model tx commits.
const (
	hookDelete = iota
	hookSetField
)

// hookCall is a deferred user-hook invocation, recorded while a model's tx is
// open and fired only after that tx commits.
type hookCall struct {
	kind      int
	model, id string
	field, to string
}

// modelResult is the outcome of one sweepModel call.
type modelResult struct {
	deleted int
	updated int
	skipped int // rows an action could no longer touch (0 rows) — idempotent no-ops
	hooks   []hookCall
}

// sweepModel sweeps one model: it claims due rows for every scheduled spec via
// the adapter's filtered read path, then applies each action inside a single
// transaction so the tick is atomic per model.
//
// Reads happen before the transaction opens (a committed snapshot); writes all
// happen inside it. Every write is idempotent — safe to re-run and to run on
// concurrent replicas — though without a Config.Locker a set-field transition
// still fires its hook once per replica that commits it.
func (r *Runner) sweepModel(ctx context.Context, meta *maniflex.ModelMeta, now time.Time) (res modelResult, err error) {
	// A panic in the adapter, MapToRecord, or a Tx op is contained here as a
	// per-model error so Sweep records it and moves on to the next model rather
	// than aborting the whole tick. The deferred tx.Rollback below is registered
	// later, so it runs first on unwind — a mid-tx panic leaves nothing partial.
	defer func() {
		if p := recover(); p != nil {
			r.cfg.Logger.Error("scheduled: model sweep panicked; recovered",
				slog.String("model", meta.Name),
				slog.Any("panic", p),
				slog.String("stack", string(debug.Stack())),
			)
			res, err = modelResult{}, fmt.Errorf("scheduled: model %s panicked: %v", meta.Name, p)
		}
	}()

	// ── Phase 1: collect due rows (reads, no tx) ──────────────────────────────
	type todo struct {
		id   string
		spec maniflex.ScheduledSpec
	}
	var work []todo

	for _, spec := range meta.Scheduled() {
		q := r.dueQuery(spec, now)
		rows, _, err := r.db.FindMany(ctx, meta, q)
		if err != nil {
			return modelResult{}, fmt.Errorf("scheduled: model %s find due rows: %w", meta.Name, err)
		}
		for _, row := range rows {
			work = append(work, todo{id: fmt.Sprint(maniflex.RecordToMap(meta, row)["id"]), spec: spec})
		}
	}
	if len(work) == 0 {
		return modelResult{}, nil
	}

	// ── Phase 2: apply every action in one transaction ────────────────────────
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return modelResult{}, fmt.Errorf("scheduled: model %s begin tx: %w", meta.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	res = modelResult{}
	for _, w := range work {
		err := r.applyAction(ctx, tx, meta, w.id, w.spec, &res)
		if errors.Is(err, maniflex.ErrNotFound) {
			// The row was already actioned earlier this tick (a delete short-
			// circuiting a later same-row spec), already soft-deleted, or removed
			// by a concurrent replica — an idempotent no-op, not a batch failure.
			// Skip it: one gone row must not roll the whole model batch back and
			// starve every other due row forever (SCHED-2).
			res.skipped++
			continue
		}
		if err != nil {
			return modelResult{}, fmt.Errorf("scheduled: model %s apply: %w", meta.Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return modelResult{}, fmt.Errorf("scheduled: model %s commit: %w", meta.Name, err)
	}
	return res, nil
}

// dueQuery builds the QueryParams that selects rows due for one spec. The
// FilterExprs are built in Go, bypassing HTTP filterability validation.
func (r *Runner) dueQuery(spec maniflex.ScheduledSpec, now time.Time) *maniflex.QueryParams {
	// Match the fixed-width form the SQL adapters store time.Time in, so the
	// lexicographic TEXT comparison of this due-check bound is correct on SQLite.
	nowVal := maniflex.CanonicalTime(now)
	filters := []*maniflex.FilterExpr{
		{Field: spec.Column, Operator: maniflex.OpLte, Value: nowVal, Group: -1},
		{Field: spec.Column, Operator: maniflex.OpNotNull, Group: -1},
	}
	if spec.Action == maniflex.SchedSetField {
		if spec.HasFrom {
			// Act only while the field still holds the guard value. Once it has
			// moved off `from`, the row no longer matches — idempotent.
			filters = append(filters, &maniflex.FilterExpr{
				Field: spec.Field, Operator: maniflex.OpEq, Value: spec.From, Group: -1,
			})
		} else {
			// No guard: skip rows already at the target value — idempotent.
			filters = append(filters, &maniflex.FilterExpr{
				Field: spec.Field, Operator: maniflex.OpNeq, Value: spec.To, Group: -1,
			})
		}
	}
	return &maniflex.QueryParams{Page: 1, Limit: r.cfg.BatchSize, Filters: filters}
}

// applyAction performs one spec's action against one row inside tx and records
// the deferred hook.
func (r *Runner) applyAction(
	ctx context.Context, tx maniflex.Tx, meta *maniflex.ModelMeta,
	id string, spec maniflex.ScheduledSpec, res *modelResult,
) error {
	switch spec.Action {
	case maniflex.SchedSoftDelete:
		if err := tx.Delete(ctx, meta, id); err != nil {
			return err
		}
		res.deleted++
		res.hooks = append(res.hooks, hookCall{kind: hookDelete, model: meta.Name, id: id})

	case maniflex.SchedHardDelete:
		hd, ok := tx.(hardDeleter)
		if !ok {
			return fmt.Errorf("transaction does not support hard delete")
		}
		if err := hd.HardDelete(ctx, meta, id); err != nil {
			return err
		}
		res.deleted++
		res.hooks = append(res.hooks, hookCall{kind: hookDelete, model: meta.Name, id: id})

	case maniflex.SchedSetField:
		data := map[string]any{spec.Field: spec.To}
		rec, _ := maniflex.MapToRecord(meta, data)
		if _, err := tx.Update(ctx, meta, id, rec, map[string]struct{}{spec.Field: {}}); err != nil {
			return err
		}
		res.updated++
		res.hooks = append(res.hooks, hookCall{
			kind: hookSetField, model: meta.Name, id: id, field: spec.Field, to: spec.To,
		})
	}
	return nil
}
