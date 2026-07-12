package sqlcore

// AutoMigrate never rewrites a column, so a changed Go type silently keeps the
// old one; the migrator now warns instead of staying quiet (BUG-16).
//
// The warning compares the model's SQL type against the type the *database*
// reports, and those are spelled differently on every driver — Postgres reports
// TIMESTAMPTZ as "timestamp with time zone" and NUMERIC(20,4) as "numeric". A
// warning that fires on a correctly-migrated table would be worse than none, so
// the no-drift half of this table matters as much as the drift half.

import "testing"

func TestSQLTypeFamily_DriftDetection(t *testing.T) {
	cases := []struct {
		name      string
		modelType string // what goTypeToSQL produces for the field
		liveType  string // what the database reports for the column
		wantDrift bool
	}{
		// Real drift — the audit's case and its neighbours.
		{"int to string, sqlite", "TEXT", "INTEGER", true},
		{"int to string, postgres", "TEXT", "bigint", true},
		{"string to int, postgres", "BIGINT", "text", true},
		{"string to bool, postgres", "BOOLEAN", "text", true},
		{"float to text, sqlite", "TEXT", "REAL", true},
		{"time to int, postgres", "BIGINT", "timestamp with time zone", true},

		// Same type, different spelling — must stay quiet.
		{"timestamptz", "TIMESTAMPTZ", "timestamp with time zone", false},
		{"varchar", "TEXT", "character varying", false},
		{"int32", "INTEGER", "integer", false},
		{"int64 postgres", "BIGINT", "bigint", false},
		{"bool postgres", "BOOLEAN", "boolean", false},
		{"float postgres", "REAL", "real", false},
		{"jsonb", "JSONB", "jsonb", false},
		{"json vs jsonb", "JSONB", "json", false},
		{"numeric loses its precision", "NUMERIC(20,4)", "numeric", false},
		{"sqlite keeps its declared type", "TEXT", "TEXT", false},
		{"sqlite bool is an integer", "INTEGER", "INTEGER", false},
		{"case and padding", "text", "  TEXT  ", false},

		// No opinion — an unrecognised type on either side never warns.
		{"unknown model type", "GEOGRAPHY(POINT)", "text", false},
		{"unknown db type", "TEXT", "tsvector", false},
		{"empty db type", "TEXT", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, live := sqlTypeFamily(c.modelType), sqlTypeFamily(c.liveType)
			drift := want != "" && live != "" && want != live
			if drift != c.wantDrift {
				t.Errorf("model %q (family %q) vs db %q (family %q): drift = %v, want %v",
					c.modelType, want, c.liveType, live, drift, c.wantDrift)
			}
		})
	}
}
