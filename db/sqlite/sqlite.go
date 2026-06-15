// Package sqlite provides a SQLite-backed maniflex.DBAdapter using modernc.org/sqlite
// (a pure-Go driver; no CGo required).
package sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"maniflex"
	"maniflex/db/sqlcore"

	_ "modernc.org/sqlite" // register "sqlite" driver
)

// connectionPragmas are appended to every DSN so they apply to every pooled
// connection. The modernc.org/sqlite driver supports `_pragma=foo(bar)` query
// parameters that the driver re-applies on every Open. journal_mode and
// synchronous are database-scoped (WAL) — issuing them on each connection is
// harmless. foreign_keys and busy_timeout are connection-scoped and MUST be
// set per-connection, which is why the previous one-shot applyPragmas was a
// bug (issue 11A.2 / checkpoint C2).
var connectionPragmas = []string{
	"_pragma=journal_mode(WAL)",
	"_pragma=synchronous(NORMAL)",
	"_pragma=foreign_keys(ON)",
	"_pragma=busy_timeout(5000)",
}

// Open opens (or creates) a SQLite database file and returns a maniflex.DBAdapter.
//
// Use ":memory:" for an in-process database (useful in tests).
//
//	db, err := sqlite.Open("./data.db", reg)
//	db, err := sqlite.Open(":memory:", reg)
func Open(path string, reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
	if path == ":memory:" {
		path = fmt.Sprintf("file:memdb%s?mode=memory&cache=shared", maniflex.RandomString(6, maniflex.DIGITS))
	}
	dsn := withPragmas(path)

	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}

	// --- Pool sizing ---
	// Readers: concurrent
	readDB.SetMaxOpenConns(10)
	readDB.SetMaxIdleConns(10)

	// Writer: strictly serialized
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	// --- Verify connections ---
	if err := readDB.Ping(); err != nil {
		return nil, fmt.Errorf("sqlite read ping: %w", err)
	}
	if err := writeDB.Ping(); err != nil {
		return nil, fmt.Errorf("sqlite write ping: %w", err)
	}

	adapter := sqlcore.New(writeDB, readDB, maniflex.SQLite, reg)
	adapter.SetErrorNormalizer(NormalizeError)
	return adapter, nil
}

// withPragmas appends connectionPragmas to the DSN as query parameters so the
// modernc.org/sqlite driver re-applies them on every new pooled connection.
// Preserves any pragmas the caller already specified (e.g. _txlock=immediate).
func withPragmas(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + strings.Join(connectionPragmas, "&")
}

// MustOpen is like Open but panics on error.
func MustOpen(path string, reg maniflex.RegistryAccessor) maniflex.DBAdapter {
	adapter, err := Open(path, reg)
	if err != nil {
		panic(err)
	}
	return adapter
}
