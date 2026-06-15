package testutil

import "testing"

// TestResolveDriver pins the selection precedence and the fail-loud behaviour:
// the -db flag wins over MANIFLEX_TEST_DB, both empty means SQLite, and an
// unknown value errors rather than silently defaulting to SQLite.
func TestResolveDriver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		flag    string
		env     string
		want    DriverKind
		wantErr bool
	}{
		{"default empty", "", "", DriverSQLite, false},
		{"env sqlite", "", "sqlite", DriverSQLite, false},
		{"env postgres", "", "postgres", DriverPostgres, false},
		{"env alias pg", "", "pg", DriverPostgres, false},
		{"env case-insensitive", "", "Postgres", DriverPostgres, false},
		{"flag overrides env", "sqlite", "postgres", DriverSQLite, false},
		{"flag postgres beats empty env", "postgres", "", DriverPostgres, false},
		{"whitespace trimmed", "  postgres  ", "", DriverPostgres, false},
		{"unknown env errors", "", "mysql", "", true},
		{"unknown flag errors", "oracle", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDriverFrom(tc.flag, tc.env)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveDriverFrom(%q,%q) = %q, want error", tc.flag, tc.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDriverFrom(%q,%q) unexpected error: %v", tc.flag, tc.env, err)
			}
			if got != tc.want {
				t.Fatalf("resolveDriverFrom(%q,%q) = %q, want %q", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}
