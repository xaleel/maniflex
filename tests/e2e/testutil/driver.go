package testutil

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// DriverKind identifies which database backend the e2e suite runs against for
// the current invocation. Selection is either-or, not both-in-one-process: a
// single `go test` run targets exactly one backend.
type DriverKind string

const (
	DriverSQLite   DriverKind = "sqlite"
	DriverPostgres DriverKind = "postgres"
)

// dbFlag is an optional convenience override for MANIFLEX_TEST_DB:
//
//	go test ./tests/e2e/... -db=postgres
//
// The flag is registered on the default flag set, so `go test` parses it for us.
// When empty (the default) the env var is consulted instead.
var dbFlag = flag.String("db", "",
	`e2e test database: "sqlite" (default) or "postgres". Overrides MANIFLEX_TEST_DB.`)

// resolveDriverFrom is the pure decision function behind Driver(): given the
// raw flag and env values it returns the selected driver, or an error for an
// unrecognised value. The flag wins over the env var; both empty means SQLite.
func resolveDriverFrom(flagVal, envVal string) (DriverKind, error) {
	raw := strings.TrimSpace(flagVal)
	if raw == "" {
		raw = strings.TrimSpace(envVal)
	}
	switch strings.ToLower(raw) {
	case "", "sqlite":
		return DriverSQLite, nil
	case "postgres", "postgresql", "pg":
		return DriverPostgres, nil
	default:
		return "", fmt.Errorf(
			"testutil: unknown test database %q — set MANIFLEX_TEST_DB or -db to %q or %q",
			raw, DriverSQLite, DriverPostgres)
	}
}

// Driver returns the database backend selected for this test invocation.
// An unrecognised MANIFLEX_TEST_DB / -db value panics rather than silently
// falling back to SQLite, so a misconfigured lane fails loudly instead of
// hiding a gap.
func Driver() DriverKind {
	d, err := resolveDriverFrom(*dbFlag, os.Getenv("MANIFLEX_TEST_DB"))
	if err != nil {
		panic(err)
	}
	return d
}

// IsPostgres reports whether the active lane is Postgres.
func IsPostgres() bool { return Driver() == DriverPostgres }

// IsSQLite reports whether the active lane is SQLite.
func IsSQLite() bool { return Driver() == DriverSQLite }
