package saga_test

import (
	"context"
	"errors"
	"testing"

	"maniflex/pkg/saga"
)

var errStep = errors.New("step error")
var errUndo = errors.New("undo error")

func noop(_ context.Context, _ saga.State) error  { return nil }
func fail(_ context.Context, _ saga.State) error  { return errStep }
func undoFail(_ context.Context, _ saga.State) error { return errUndo }

func TestAllStepsSucceed(t *testing.T) {
	var order []string
	make := func(name string) saga.StepFn {
		return func(_ context.Context, s saga.State) error {
			order = append(order, name)
			s[name] = true
			return nil
		}
	}

	s := saga.New("test").
		Step("a", make("a"), nil).
		Step("b", make("b"), nil).
		Step("c", make("c"), nil)

	st := saga.State{}
	if err := s.Execute(context.Background(), st); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("unexpected execution order: %v", order)
	}
	if !st["a"].(bool) || !st["b"].(bool) || !st["c"].(bool) {
		t.Fatal("state not populated")
	}
}

func TestStepNFailsUndoRunsInReverse(t *testing.T) {
	var undone []string
	makeUndo := func(name string) saga.StepFn {
		return func(_ context.Context, _ saga.State) error {
			undone = append(undone, name)
			return nil
		}
	}

	s := saga.New("test").
		Step("a", noop, makeUndo("a")).
		Step("b", noop, makeUndo("b")).
		Step("c", fail, makeUndo("c")) // c fails → c's undo NOT called

	err := s.Execute(context.Background(), saga.State{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, errStep) {
		t.Fatalf("expected errStep, got %v", err)
	}
	// undo for a and b should run in reverse; c's undo must NOT run
	if len(undone) != 2 || undone[0] != "b" || undone[1] != "a" {
		t.Fatalf("unexpected undo order: %v", undone)
	}
}

func TestUndoFailureWrapped(t *testing.T) {
	s := saga.New("test").
		Step("a", noop, undoFail).
		Step("b", fail, nil)

	err := s.Execute(context.Background(), saga.State{})
	if err == nil {
		t.Fatal("expected error")
	}

	var ue *saga.UndoError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UndoError, got %T: %v", err, err)
	}
	if !errors.Is(ue.Cause, errStep) {
		t.Fatalf("cause should be errStep, got %v", ue.Cause)
	}
	if len(ue.Failures) != 1 || !errors.Is(ue.Failures[0].Err, errUndo) {
		t.Fatalf("unexpected undo failures: %v", ue.Failures)
	}
	// Unwrap should return original cause
	if !errors.Is(err, errStep) {
		t.Fatal("errors.Is(err, errStep) should be true via Unwrap")
	}
}

func TestEmptySaga(t *testing.T) {
	err := saga.New("empty").Execute(context.Background(), saga.State{})
	if err != nil {
		t.Fatalf("empty saga should succeed, got %v", err)
	}
}

func TestContextCancelledBeforeExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	s := saga.New("test").Step("a", func(_ context.Context, _ saga.State) error {
		called = true
		return nil
	}, nil)

	err := s.Execute(ctx, saga.State{})
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if called {
		t.Fatal("step should not have been called")
	}
}

func TestContextCancelledBetweenSteps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var undone []string

	s := saga.New("test").
		Step("a", noop, func(_ context.Context, _ saga.State) error {
			undone = append(undone, "a")
			return nil
		}).
		Step("b", func(_ context.Context, _ saga.State) error {
			cancel() // cancel after b starts
			return nil
		}, func(_ context.Context, _ saga.State) error {
			undone = append(undone, "b")
			return nil
		}).
		Step("c", noop, func(_ context.Context, _ saga.State) error {
			undone = append(undone, "c")
			return nil
		})

	err := s.Execute(ctx, saga.State{})
	if err == nil {
		t.Fatal("expected error after cancel")
	}
	// a and b completed before the cancel was checked; c was skipped
	// undo order: b then a
	if len(undone) != 2 || undone[0] != "b" || undone[1] != "a" {
		t.Fatalf("unexpected undo order after cancel: %v", undone)
	}
}
