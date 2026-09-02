package sqlcore

// `CREATE TABLE IF NOT EXISTS` is not race-safe on Postgres, and neither is
// `CREATE INDEX IF NOT EXISTS`. Both check the catalog and then insert into it,
// so two replicas booting together can both pass the check; the loser's insert
// collides on a catalog index and it is told
//
//	duplicate key value violates unique constraint "pg_type_typname_nsp_index"
//
// Postgres documents the window and does not close it. This is a third instance
// of the same shape as isDuplicateColumnError and isDuplicateConstraintError,
// with one difference that decides the fix: the whole model migrates inside one
// transaction, and a failed statement aborts it — so the loser cannot shrug the
// error off where it happens and carry on. It has to roll back and start again.
//
//	go test ./db/sqlcore/ -run 'ConcurrentDDL|MigrateWithRetry'

import (
	"errors"
	"testing"
)

func TestIsConcurrentDDLError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
		why  string
	}{
		{
			name: "concurrent CREATE TABLE, verbatim from CI",
			err: errors.New(`migrate FKRaceAuthor: create table fk_race_authors: ` +
				`pq: duplicate key value violates unique constraint "pg_type_typname_nsp_index" (23505)`),
			want: true,
			why:  "the table's implicit row type is what the loser collides on",
		},
		{
			name: "concurrent CREATE TABLE hitting pg_class instead",
			err: errors.New(
				`pq: duplicate key value violates unique constraint "pg_class_relname_nsp_index"`),
			want: true,
			why:  "the same race can surface on either catalog index",
		},
		{
			name: "concurrent CREATE INDEX IF NOT EXISTS",
			err: errors.New(`ERROR: duplicate key value violates unique constraint ` +
				`"pg_class_relname_nsp_index" (SQLSTATE 23505)`),
			want: true,
			why:  "an index is a pg_class row too, and its IF NOT EXISTS races the same way",
		},
		{
			// The unique-index path wraps the driver error in advice about
			// de-duplicating rows. The race has to be recognised through that
			// wrapper, or a lost race is reported to the operator as dirty data.
			name: "seen through uniqueIndexFailure's wrapping",
			err: uniqueIndexFailure("orders", "uidx_orders_email", []string{"email"},
				errors.New(`pq: duplicate key value violates unique constraint "pg_class_relname_nsp_index"`)),
			want: true,
			why:  "%w keeps the driver's text, which is what the match reads",
		},
		{
			// The same race, reported the other way. Which of the two a losing
			// replica gets depends on how far the winner had got: a collision on
			// the catalog insert is 23505, and finding the winner's committed row
			// first is 42710. Twelve runs of the e2e race test produced both.
			name: "concurrent CREATE TABLE reported as duplicate_object (42710)",
			err:  errors.New(`pq: type "fk_race_books" already exists (42710)`),
			want: true,
			why:  "the type named is the table's own implicit row type",
		},
		{
			name: "pgx wording for the same thing",
			err:  errors.New(`ERROR: type "orders" already exists (SQLSTATE 42710)`),
			want: true,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			// The migration backfills and creates unique indexes over existing
			// rows. A duplicate in the user's own data is a real failure and
			// retrying it would only fail again, more slowly.
			name: "unique violation on the user's own data",
			err: errors.New(
				`pq: duplicate key value violates unique constraint "orders_email_key"`),
			want: false,
			why:  "not a catalog index: the data is what is duplicated",
		},
		{
			name: "unrelated failure",
			err:  errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
			want: false,
		},
		{
			// This case was written asserting false, on the reasoning that every
			// CREATE here carries IF NOT EXISTS so a relation-already-exists could
			// only mean something else was wrong. That reasoning was wrong, and 60
			// runs of the four-replica test falsified it: IF NOT EXISTS re-checks
			// the catalog under the covers and reports 42P07 when the winner
			// committed in the window. It is the third form of the one race.
			name: "concurrent CREATE TABLE reported as duplicate_table (42P07)",
			err:  errors.New(`pq: relation "fk_race_books" already exists (42P07)`),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isConcurrentDDLError(tc.err); got != tc.want {
				t.Errorf("isConcurrentDDLError(%v) = %v, want %v \u2014 %s", tc.err, got, tc.want, tc.why)
			}
		})
	}
}

var errLostDDLRace = errors.New(
	`pq: duplicate key value violates unique constraint "pg_type_typname_nsp_index"`)

func TestMigrateWithRetry(t *testing.T) {
	t.Parallel()

	// Each attempt is a fresh transaction: the aborted one has to be rolled back
	// before anything else can run on that connection, which is why the retry
	// lives at the transaction boundary rather than around the statement.
	t.Run("retries once and succeeds", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := migrateWithRetry(func() error {
			attempts++
			if attempts == 1 {
				return errLostDDLRace
			}
			return nil
		})
		if err != nil {
			t.Errorf("migrateWithRetry: %v", err)
		}
		if attempts != 2 {
			t.Errorf("attempts = %d, want 2", attempts)
		}
	})

	// With several replicas a retry can lose again on a different object — the
	// table the first time, an index the second — so one retry is not enough.
	t.Run("survives losing more than one race", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := migrateWithRetry(func() error {
			attempts++
			if attempts < migrateRaceAttempts {
				return errLostDDLRace
			}
			return nil
		})
		if err != nil {
			t.Errorf("migrateWithRetry: %v", err)
		}
		if attempts != migrateRaceAttempts {
			t.Errorf("attempts = %d, want %d", attempts, migrateRaceAttempts)
		}
	})

	// Bounded: a race that never resolves is a bug, and looping on it would turn
	// a failed boot into a hung one.
	t.Run("gives up and reports the last failure", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		err := migrateWithRetry(func() error {
			attempts++
			return errLostDDLRace
		})
		if !errors.Is(err, errLostDDLRace) {
			t.Errorf("err = %v, want the race error to survive", err)
		}
		if attempts != migrateRaceAttempts {
			t.Errorf("attempts = %d, want %d", attempts, migrateRaceAttempts)
		}
	})

	// A real migration failure must fail the boot at once. Retrying it would
	// report the same error later, having issued the same broken DDL again.
	t.Run("does not retry an ordinary failure", func(t *testing.T) {
		t.Parallel()
		boom := errors.New(`pq: column "total" of relation "orders" contains null values`)
		attempts := 0
		err := migrateWithRetry(func() error {
			attempts++
			return boom
		})
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1: an ordinary failure is not a race", attempts)
		}
	})

	t.Run("does not retry a success", func(t *testing.T) {
		t.Parallel()
		attempts := 0
		if err := migrateWithRetry(func() error { attempts++; return nil }); err != nil {
			t.Errorf("migrateWithRetry: %v", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1", attempts)
		}
	})
}

// Three error forms for one race, each found only by running the four-replica
// test enough times, is evidence that recognising the symptom is the wrong
// primary defence. The lock removes the race instead: replicas migrating the
// same table take the same advisory key and go one at a time, so the second one
// finds the table present and every IF NOT EXISTS is the no-op it was meant to
// be. isConcurrentDDLError stays as the fallback for a replica that does not
// take the lock — a rolling upgrade from v0.12.0 or earlier is exactly that.
//
// The key has to be a pure function of the table name and nothing else: two
// replicas are two processes, and a key that disagreed between them would
// serialise nothing while looking like it did.
func TestMigrateLockKey(t *testing.T) {
	t.Parallel()

	if migrateLockKey("orders") == migrateLockKey("order_items") {
		t.Error("two tables share a key: unrelated migrations would serialise")
	}
	// The value is pinned rather than compared against a second call in the same
	// process, which would prove nothing — the agreement that matters is between
	// separate replicas, possibly on different maniflex versions during a rolling
	// upgrade. A pinned constant is what holds them to the same key; changing the
	// derivation has to be a deliberate act that updates this line.
	if got, want := migrateLockKey("orders"), int64(-4739343157541256222); got != want {
		t.Errorf("migrateLockKey(\"orders\") = %d, want %d", got, want)
	}
}
