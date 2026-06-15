// Package saga provides a lightweight in-process saga coordinator for
// cross-domain atomic operations. It executes a sequence of steps and runs
// compensation (undo) functions in reverse order if any step fails.
//
// It is NOT crash-safe: for durable sagas, wrap execution inside a pkg/jobs job.
//
// Example:
//
//	err := saga.New("dispense_charge").
//	    Step("create_invoice",  createInvoice, voidInvoice).
//	    Step("debit_ar_ledger", debitAR,       reverseAR).
//	    Step("deduct_stock",    deductStock,   restock).
//	    Execute(ctx, saga.State{"patient_id": pid, "items": items})
package saga

import (
	"context"
	"log/slog"
)

// State is the shared mutable map passed through every step. Steps read
// their inputs from it and write outputs into it so later steps can consume
// them. State is NOT safe for concurrent access; steps run sequentially.
type State map[string]any

// StepFn is a function that executes one saga step.
type StepFn func(ctx context.Context, state State) error

type step struct {
	name       string
	do         StepFn
	undo       StepFn
	idempotent bool
}

// Saga is a named sequence of steps with compensating actions.
type Saga struct {
	name   string
	steps  []step
	logger *slog.Logger
}

// New creates a new Saga with the given name.
func New(name string) *Saga {
	return &Saga{name: name}
}

// WithLogger sets a logger for step-level diagnostics. Defaults to slog.Default().
func (s *Saga) WithLogger(l *slog.Logger) *Saga {
	s.logger = l
	return s
}

// Step appends a named step with its do and undo functions.
// undo may be nil if the step has no compensation logic.
func (s *Saga) Step(name string, do, undo StepFn) *Saga {
	s.steps = append(s.steps, step{name: name, do: do, undo: undo})
	return s
}

// Idempotent marks the most recently added step as safe to retry once inline.
// Reserved for future use; v1 does not retry automatically.
func (s *Saga) Idempotent() *Saga {
	if len(s.steps) > 0 {
		s.steps[len(s.steps)-1].idempotent = true
	}
	return s
}

func (s *Saga) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// Execute runs all steps in order. If a step returns an error, execution stops
// and completed steps are compensated in reverse order (the failing step's undo
// is NOT called). If any undo also fails, an UndoError is returned wrapping the
// original cause and all undo failures.
//
// If ctx is already cancelled before execution begins, ctx.Err() is returned
// immediately. Context cancellation between steps is checked before each step.
func (s *Saga) Execute(ctx context.Context, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	log := s.log()
	var completed []step

	for _, st := range s.steps {
		if err := ctx.Err(); err != nil {
			return s.compensate(completed, state, err)
		}

		log.Info("saga step start", "saga", s.name, "step", st.name)
		if err := st.do(ctx, state); err != nil {
			log.Error("saga step failed", "saga", s.name, "step", st.name, "error", err)
			return s.compensate(completed, state, err)
		}
		log.Info("saga step done", "saga", s.name, "step", st.name)
		completed = append(completed, st)
	}

	return nil
}

// compensate runs undo functions for completed steps in reverse order.
func (s *Saga) compensate(completed []step, state State, cause error) error {
	log := s.log()
	var failures []UndoFailure

	for i := len(completed) - 1; i >= 0; i-- {
		st := completed[i]
		if st.undo == nil {
			continue
		}
		log.Info("saga undo start", "saga", s.name, "step", st.name)
		if err := st.undo(context.Background(), state); err != nil {
			log.Error("saga undo failed", "saga", s.name, "step", st.name, "error", err)
			failures = append(failures, UndoFailure{Step: st.name, Err: err})
		} else {
			log.Info("saga undo done", "saga", s.name, "step", st.name)
		}
	}

	if len(failures) > 0 {
		return &UndoError{Cause: cause, Failures: failures}
	}
	return cause
}
