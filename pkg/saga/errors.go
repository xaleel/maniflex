package saga

import (
	"fmt"
	"strings"
)

// UndoFailure records one compensation step that failed during rollback.
type UndoFailure struct {
	Step string
	Err  error
}

// UndoError is returned when the original step error was accompanied by one or
// more failures in the compensation chain. Cause is the original step error;
// Failures lists every undo that also errored.
type UndoError struct {
	Cause    error
	Failures []UndoFailure
}

func (e *UndoError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "saga failed: %v; undo errors:", e.Cause)
	for _, f := range e.Failures {
		fmt.Fprintf(&sb, " [%s: %v]", f.Step, f.Err)
	}
	return sb.String()
}

func (e *UndoError) Unwrap() error { return e.Cause }
