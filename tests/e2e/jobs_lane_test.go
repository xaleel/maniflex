package e2e

// Lane helpers for the jobs/sql tests.
//
// jobs/sql speaks two dialects and the tests around it have to follow: the
// migration needs the driver name the lane is running, and any raw verification
// query written with SQLite's "?" placeholders has to be rebound to Postgres's
// $1..$n. Without this the whole jobs suite fails on Postgres with
// `pq: syntax error at end of input` — a test-side artifact that looks
// alarmingly like a library fault.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// jobsDriver is the driver name jobssql.Migrate expects for the active lane.
func jobsDriver() string {
	if testutil.IsPostgres() {
		return "postgres"
	}
	return "sqlite"
}

// ph rebinds a query written with "?" placeholders to the active lane's style.
// It is for the tests' own verification queries; jobs/sql builds its statements
// with its internal placeholder helper and never needs this.
//
// Naive on purpose: it rewrites every "?" outside a single-quoted literal, which
// is all these fixture queries contain.
func ph(query string) string {
	if !testutil.IsPostgres() {
		return query
	}
	var b strings.Builder
	n := 0
	inLiteral := false
	for _, r := range query {
		switch {
		case r == '\'':
			inLiteral = !inLiteral
			b.WriteRune(r)
		case r == '?' && !inLiteral:
			n++
			b.WriteString("$" + strconv.Itoa(n))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipUnlessSQLite marks a test that asserts a SQLite-specific storage property.
// The lexicographic-timestamp tests are the case that matters: SQLite compares
// the timestamp columns as TEXT, which is why the fixed-width format exists,
// whereas Postgres stores them as TIMESTAMPTZ and orders them natively. Running
// those assertions on Postgres would not test the same thing.
func skipUnlessSQLite(t *testing.T, why string) {
	t.Helper()
	if testutil.IsPostgres() {
		t.Skipf("SQLite-lane test: %s", why)
	}
}

// parseStampColumn reads a timestamp column that both lanes spell differently:
// SQLite stores it as fixed-width TEXT, Postgres as TIMESTAMPTZ, so the driver
// hands back a string in one lane and a time.Time in the other. ok is false when
// the column is NULL.
func parseStampColumn(t *testing.T, v any) (time.Time, bool) {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return time.Time{}, false
	case time.Time:
		return x, true
	case string:
		return mustParseStamp(t, x)
	case []byte:
		return mustParseStamp(t, string(x))
	default:
		t.Fatalf("unexpected type %T for a timestamp column", v)
		return time.Time{}, false
	}
}

func mustParseStamp(t *testing.T, s string) (time.Time, bool) {
	t.Helper()
	if s == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse timestamp %q: %v", s, err)
	}
	return parsed, true
}
