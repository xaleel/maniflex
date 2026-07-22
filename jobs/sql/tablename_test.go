package sql

// Audit JB-13: WithTableName is interpolated straight into every query and into
// the migration DDL, with no identifier validation. A table reference cannot be a
// bind parameter, so the name is substituted textually — in queue.go's q() into
// the quoted form ("job_queue" → "<name>"), and in migrate.go's rename() as a
// bare substring, which is also how the index names are derived. A name carrying
// a double quote therefore closes the quoted identifier and leaves the remainder
// as SQL, and the bare substitution in the index names cannot be quote-escaped at
// all. The name is now required to be a plain SQL identifier.
//
//	go test ./jobs/sql/ -run TestTableName

import (
	"context"
	"strings"
	"testing"
)

func TestTableName_ValidNamesAccepted(t *testing.T) {
	for _, name := range []string{
		"job_queue",  // the default
		"otp_jobs",   // the documented use: a separate lane
		"_private",   // leading underscore is a legal identifier start
		"A1",         // digits after the first character
		"Q",          // single character
		"jobs_v2_eu", // digits and underscores throughout
	} {
		if _, err := newConfig([]Option{WithTableName(name)}); err != nil {
			t.Errorf("valid table name %q rejected: %v", name, err)
		}
	}
}

// An empty name is not an error: it falls back to the default, which is the
// pre-existing behaviour and is checked before validation.
func TestTableName_EmptyFallsBackToDefault(t *testing.T) {
	c, err := newConfig([]Option{WithTableName("")})
	if err != nil {
		t.Fatalf("empty name should fall back to the default, got: %v", err)
	}
	if c.table != defaultTableName {
		t.Errorf("empty name gave table %q, want the default %q", c.table, defaultTableName)
	}
}

// Each of these would be substituted textually into the SQL. The first is the
// one that matters: it closes the quoted identifier and appends a statement.
func TestTableName_InjectionAndMalformedRejected(t *testing.T) {
	for _, name := range []string{
		`job_queue" ; DROP TABLE users; --`, // breaks out of the quoted identifier
		`job_queue"`,                        // a bare trailing quote is enough
		`jobs; DELETE FROM job_queue`,       // statement separator
		`jobs--comment`,                     // comment introducer
		`jobs bar`,                          // whitespace
		`jobs-bar`,                          // not an identifier character
		`1jobs`,                             // identifiers cannot start with a digit
		`jobs.other`,                        // schema-qualified: not a bare identifier
		`jöbs`,                              // non-ASCII
		`   `,                               // whitespace only (not empty, so no fallback)
	} {
		if _, err := newConfig([]Option{WithTableName(name)}); err == nil {
			t.Errorf("table name %q was accepted — it is interpolated into every statement", name)
		}
	}
}

// The error names the offending value and the grammar, so the fix is obvious.
func TestTableName_ErrorIsActionable(t *testing.T) {
	_, err := newConfig([]Option{WithTableName(`bad-name`)})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{`bad-name`, tableIdentPattern} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// New has no error return, so it panics rather than building a Queue that would
// interpolate the bad name into every statement it ever issues.
func TestTableName_NewPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Error("New accepted an invalid table name instead of panicking")
			return
		}
		// It must be the validation panic specifically. The nil handle below would
		// itself panic once New got as far as inspecting the driver, so asserting
		// only "it panicked" would pass with the validation removed.
		err, ok := r.(error)
		if !ok || !strings.Contains(err.Error(), "invalid table name") {
			t.Errorf("New panicked with %v, want the invalid-table-name error", r)
		}
	}()
	_ = New(nil, WithTableName(`x"; DROP TABLE users; --`))
}

// Migrate has an error return, so it uses it. Validation happens before the DB is
// touched, which is why a nil handle is safe here.
func TestTableName_MigrateReturnsError(t *testing.T) {
	err := Migrate(context.Background(), nil, "sqlite", WithTableName(`x"; DROP TABLE users; --`))
	if err == nil {
		t.Fatal("Migrate accepted an invalid table name")
	}
	if !strings.Contains(err.Error(), "invalid table name") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Anti-over-reach: validation must not break the feature it guards — a valid
// custom name still rewrites the table reference in the generated SQL.
func TestTableName_ValidNameStillRewritesSQL(t *testing.T) {
	q := &Queue{table: "otp_jobs"}
	got := q.q(`SELECT "id" FROM "job_queue" WHERE "status"='enqueued'`)
	want := `SELECT "id" FROM "otp_jobs" WHERE "status"='enqueued'`
	if got != want {
		t.Errorf("q() = %q, want %q", got, want)
	}
}
