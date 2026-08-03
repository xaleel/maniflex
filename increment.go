package maniflex

// increment.go — atomic column arithmetic (audit WR-2).
//
// Bumping a counter through the ordinary write path is read-then-write: fetch
// the row, add one in Go, send the new absolute value back. Two requests that
// read the same value both write the same value, and one of the increments is
// gone — a lost update, with nothing anywhere reporting it. It is the reason
// this primitive exists, and the reason it cannot be built on top of Update:
// Update sends a value, and the whole point is to send an *operation* the
// database applies to whatever it currently holds.
//
// The guard is half the feature. "Decrement stock, but never below zero" is the
// shape almost every real counter has, and checking it in Go before calling
// Increment reintroduces exactly the race the increment removed. So the bound
// travels into the statement's WHERE clause, and the write either happens
// completely or not at all.
//
// Bounds come from the field's existing mfx:"min:"/"max:" tags rather than a new
// argument. They already mean "this column may not leave this range", the
// validator already enforces them on ordinary writes, and taking them from the
// declaration means one statement of the invariant instead of one per call site
// — a call site that forgets is the failure mode a per-call bound would have.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
)

// ErrIncrementOutOfBounds is returned when an increment would take a column
// outside the range its mfx:"min:"/"max:" tags declare.
//
// It is distinct from ErrConstraint because nothing in the database was
// violated: the row still satisfies every constraint it has, and the framework
// declined to make the write. It is also distinct from ErrNotFound, which the
// same zero-rows-affected result would otherwise be indistinguishable from —
// telling them apart matters, since one means "no such row" and the other means
// "that row cannot go that low right now", and only the second may succeed on a
// retry.
var ErrIncrementOutOfBounds = errors.New("maniflex: increment would leave the column's declared bounds")

// ErrIncrementNotSupported is returned when the active adapter does not
// implement Incrementer.
//
// The alternative — quietly doing a read, adding in Go, and writing the sum —
// is refused on purpose. It would return the right answer under test and lose
// updates under load, which is the failure this method exists to prevent, and
// the caller would have no way to tell which one they were getting.
var ErrIncrementNotSupported = errors.New(
	"maniflex: the configured database adapter does not support atomic increments")

// Incrementer is an optional extension a DBAdapter (and its Tx) may implement to
// apply arithmetic to numeric columns in a single statement.
//
// It is separate from DBAdapter so that adding it is not a breaking change for
// third-party adapters, in the same way as Restorer and ScopeChecker. An adapter
// that does not implement it makes ModelAccessor.Increment return an error
// naming the gap. It deliberately does NOT fall back to read-then-write: a
// silent fallback would put back the lost-update race the caller reached for
// this method to avoid, and would do it invisibly.
//
// The bundled SQLite and Postgres adapters implement it via db/sqlcore.
type Incrementer interface {
	// Increment applies deltas to the row identified by id, atomically, and
	// returns the updated record as a typed *T.
	//
	// Each key is a DB column name and each value the amount to add; a negative
	// value subtracts. Every column must be numeric. updated_at is bumped, as it
	// is by Update.
	//
	// bounds carries the min/max the statement must guarantee, keyed by column,
	// derived from the model's tags by IncrementBounds. The implementation must
	// apply them inside the same statement — as WHERE conditions on the
	// post-increment value — not as a separate check.
	//
	// q carries the request's forced filters, the server-imposed scope from
	// db.Tenancy or db.ForceFilter, which must be applied so a caller cannot
	// bump a counter outside their scope by knowing its id. It is nil when
	// nothing is scoped.
	//
	// Return ErrNotFound when no row matches, and ErrIncrementOutOfBounds when a
	// row matched but a bound would have been crossed.
	Increment(ctx context.Context, model *ModelMeta, id string,
		deltas map[string]any, bounds map[string]NumericBounds, q *QueryParams) (any, error)
}

// NumericBounds is the range a column may not leave, as declared by its
// mfx:"min:"/"max:" tags. A nil member means that side is unbounded.
type NumericBounds struct {
	Min *float64
	Max *float64
}

// IncrementBounds returns the bounds to enforce for the given columns, taken
// from each field's mfx:"min:"/"max:" tags. Columns with neither are omitted, so
// an empty result means the statement needs no guard at all.
func IncrementBounds(model *ModelMeta, deltas map[string]any) map[string]NumericBounds {
	var out map[string]NumericBounds
	for col := range deltas {
		f := model.FieldByDBName(col)
		if f == nil || (f.Tags.Min == nil && f.Tags.Max == nil) {
			continue
		}
		if out == nil {
			out = make(map[string]NumericBounds, len(deltas))
		}
		out[col] = NumericBounds{Min: f.Tags.Min, Max: f.Tags.Max}
	}
	return out
}

// validateDeltas checks an increment's columns and amounts against the model
// before any statement is built.
//
// It fails closed on an unknown column rather than ignoring it, for the reason
// the filter path does: a misspelt column that is silently dropped turns "add
// one to the counter" into a statement that bumps updated_at and nothing else,
// and the caller is told it succeeded.
func validateDeltas(model *ModelMeta, deltas map[string]any) error {
	if len(deltas) == 0 {
		return errors.New("maniflex: Increment: no columns given")
	}
	// Sorted so a model with several bad columns reports them in a stable order
	// rather than whichever the map happened to yield first.
	cols := make([]string, 0, len(deltas))
	for col := range deltas {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	for _, col := range cols {
		f := model.FieldByDBName(col)
		if f == nil {
			if jf := model.FieldByJSONName(col); jf != nil {
				return fmt.Errorf(
					"maniflex: Increment: model %q has no column %q — did you mean %q? (Increment takes DB column names)",
					model.Name, col, jf.Tags.DBName)
			}
			return fmt.Errorf("maniflex: Increment: model %q has no column %q",
				model.Name, col)
		}
		if !isNumericKind(f.Type) {
			return fmt.Errorf(
				"maniflex: Increment: model %q column %q is %s, which has nothing to add to",
				model.Name, col, f.Type)
		}
		if f.Tags.Encrypted {
			// The stored value is ciphertext. SQL arithmetic on it would produce
			// a number unrelated to the plaintext, and the row would decrypt to
			// nonsense afterwards — silently, since nothing downstream can tell
			// a corrupted plaintext from a real one.
			return fmt.Errorf(
				"maniflex: Increment: model %q column %q is mfx:\"encrypted\" — the database "+
					"holds ciphertext, so it cannot be added to; read, change and write it instead",
				model.Name, col)
		}
		if !isNumericDelta(deltas[col]) {
			return fmt.Errorf(
				"maniflex: Increment: model %q column %q was given a %T, which is not a number",
				model.Name, col, deltas[col])
		}
	}
	return nil
}

// isNumericDelta reports whether v is an integer or float the statement can bind
// as an amount. Integer deltas are kept as integers all the way to the driver,
// so a counter past 2^53 stays exact — which is why this does not simply convert
// everything to float64.
func isNumericDelta(v any) bool {
	if v == nil {
		return false
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
