package sqlcore

// GAP-10. ?count=false is only worth having if it removes the query, not just
// the number, so the test has to observe the absence of a database round trip.
//
// It does that with an Adapter that has no database at all: readDb is nil, so
// any statement this path tries to issue is a nil-pointer dereference. Returning
// -1 with a nil error from an adapter in that state is proof that nothing was
// asked of the database.
//
//	go test ./db/sqlcore/ -run TestCountMatches

import (
	"context"
	"testing"

	"github.com/xaleel/maniflex"
)

func countTestModel() *maniflex.ModelMeta {
	return &maniflex.ModelMeta{Name: "Order", TableName: "orders"}
}

// dblessAdapter has no readDb: it can build SQL but cannot run any.
func dblessAdapter() *Adapter {
	return &Adapter{driver: maniflex.SQLite}
}

func TestCountMatches_SkipsTheQueryWhenTheClientDeclinedIt(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the count query ran despite ?count=false — it reached a nil database "+
				"and panicked (%v). Suppressing the total only matters if the COUNT over the "+
				"whole filtered set goes with it", r)
		}
	}()

	total, err := dblessAdapter().countMatches(context.Background(), countTestModel(),
		&maniflex.QueryParams{Page: 1, Limit: 20, SkipCount: true}, "")
	if err != nil {
		t.Fatalf("countMatches: %v", err)
	}
	if total != -1 {
		t.Errorf("total = %d, want -1 (the sentinel for \"not counted\"); 0 would read as "+
			"an empty result set", total)
	}
}

// Cursor mode has skipped the count since keyset pagination landed. The same
// gate now carries both reasons, so it is worth pinning that it still does.
func TestCountMatches_SkipsTheQueryInCursorMode(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the count query ran in cursor mode (%v)", r)
		}
	}()

	total, err := dblessAdapter().countMatches(context.Background(), countTestModel(),
		&maniflex.QueryParams{Limit: 20, Cursor: &maniflex.CursorParams{Field: "seq"}}, "")
	if err != nil {
		t.Fatalf("countMatches: %v", err)
	}
	if total != -1 {
		t.Errorf("total = %d, want -1", total)
	}
}

// The other direction: an ordinary offset list must still reach the database.
// Without this, a gate that skipped the count unconditionally would pass the two
// tests above and silently strip meta.total from every list response.
func TestCountMatches_RunsTheQueryForAnOrdinaryList(t *testing.T) {
	t.Parallel()

	reached := false
	func() {
		defer func() { reached = recover() != nil }()
		_, _ = dblessAdapter().countMatches(context.Background(), countTestModel(),
			&maniflex.QueryParams{Page: 1, Limit: 20}, "")
	}()

	if !reached {
		t.Error("a list that asked for its total did not go to the database: the count " +
			"is being skipped for every request, not only the ones that declined it")
	}
}

// A list served inside a transaction goes through txAdapter, not Adapter, so
// the gate exists twice and has to be pinned twice. Same trick: a txAdapter
// with a nil *sql.Tx cannot issue a statement without dereferencing it.
func TestCountMatches_TxSkipsTheQueryToo(t *testing.T) {
	t.Parallel()

	tx := &txAdapter{driver: maniflex.SQLite}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the count query ran inside a transaction despite ?count=false (%v)", r)
			}
		}()
		total, err := tx.countMatches(context.Background(), countTestModel(),
			&maniflex.QueryParams{Page: 1, Limit: 20, SkipCount: true}, "")
		if err != nil {
			t.Fatalf("countMatches: %v", err)
		}
		if total != -1 {
			t.Errorf("total = %d, want -1", total)
		}
	}()

	reached := false
	func() {
		defer func() { reached = recover() != nil }()
		_, _ = tx.countMatches(context.Background(), countTestModel(),
			&maniflex.QueryParams{Page: 1, Limit: 20}, "")
	}()
	if !reached {
		t.Error("a counted list inside a transaction did not go to the database")
	}
}
