// Package postgres provides a PostgreSQL-backed maniflex.DBAdapter with separate
// connection pools for writes (primary) and reads (replica/primary).
package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"maniflex"
	"maniflex/db/sqlcore"

	"github.com/lib/pq"
)

// ── Pool configuration ────────────────────────────────────────────────────────

// PoolConfig controls connection pool behaviour for one *sql.DB pool.
// Zero values are replaced by the defaults documented on each field.
type PoolConfig struct {
	// MaxOpenConns is the maximum number of open connections in the pool.
	// Default: 20 for the write pool, 40 for the read pool.
	MaxOpenConns int

	// MaxIdleConns is the maximum number of idle connections retained.
	// Should be ≤ MaxOpenConns. Default: half of MaxOpenConns.
	MaxIdleConns int

	// ConnMaxLifetime is the maximum time a connection may be reused before
	// being closed and re-opened. Prevents stale connections to PgBouncer or
	// load-balanced replicas. Default: 30 minutes.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime is the maximum time a connection may sit idle before
	// being closed. Prevents the Postgres server from closing idle connections
	// (tcp_keepalives) before the client notices. Default: 5 minutes.
	ConnMaxIdleTime time.Duration
}

// DefaultSchema is the PostgreSQL schema connections use when
// SessionConfig.SchemaName is unset. "public" is present in every database, so
// the zero-config Open path works without provisioning a schema first. A custom
// schema supplied via SchemaName is created on connect when missing — see
// ensureSchema.
var DefaultSchema = "public"

func (c PoolConfig) withDefaults(isWriter bool) PoolConfig {
	if c.MaxOpenConns == 0 {
		if isWriter {
			c.MaxOpenConns = 20
		} else {
			c.MaxOpenConns = 40
		}
	}
	if c.MaxIdleConns == 0 {
		c.MaxIdleConns = c.MaxOpenConns / 2
	}
	if c.ConnMaxLifetime == 0 {
		c.ConnMaxLifetime = 30 * time.Minute
	}
	if c.ConnMaxIdleTime == 0 {
		c.ConnMaxIdleTime = 5 * time.Minute
	}
	return c
}

// ── Session configuration ─────────────────────────────────────────────────────

// SessionConfig holds PostgreSQL session-level GUC (Grand Unified
// Configuration) parameters that are SET on every new connection.
// Zero values are replaced by the documented defaults.
type SessionConfig struct {
	// StatementTimeout is the maximum execution time for a single SQL
	// statement. Statements that exceed this are cancelled by the server.
	// Default: 30s. Set to 0 to use the server default (no timeout).
	StatementTimeout time.Duration

	// LockTimeout is the maximum time to wait for a lock before aborting.
	// Prevents lock-queue pile-ups during migrations or bulk writes.
	// Default: 5s. Set to 0 to use the server default.
	LockTimeout time.Duration

	// IdleInTransactionTimeout aborts transactions that have been idle for
	// longer than this duration — a safeguard against hung application code
	// that opens a transaction and never commits or rolls back.
	// Default: 60s. Set to 0 to use the server default.
	IdleInTransactionTimeout time.Duration

	// ApplicationName is reported in pg_stat_activity and server logs,
	// making it easy to identify connections from this service.
	// Default: "maniflex".
	ApplicationName string

	// TimeZone is the session time zone. Affects how TIMESTAMPTZ values
	// are displayed and how bare TIMESTAMP literals are interpreted.
	// Default: "UTC".
	TimeZone string

	// SchemaName scopes every pooled connection to a PostgreSQL schema via
	//   SET search_path TO <SchemaName>;
	// When the named schema does not exist it is created on connect (see
	// OpenWithConfig → ensureSchema) so AutoMigrate's DDL has somewhere to land.
	// The name must be a plain identifier ([A-Za-z_][A-Za-z0-9_$]*) — it is
	// embedded directly into SET search_path / CREATE SCHEMA.
	// Default: DefaultSchema ("public").
	SchemaName *string
}

// schema returns the effective search_path schema: SchemaName when set and
// non-empty, otherwise DefaultSchema.
func (s SessionConfig) schema() string {
	if s.SchemaName != nil && *s.SchemaName != "" {
		return *s.SchemaName
	}
	return DefaultSchema
}

func (s SessionConfig) withDefaults() SessionConfig {
	if s.StatementTimeout == 0 {
		s.StatementTimeout = 30 * time.Second
	}
	if s.LockTimeout == 0 {
		s.LockTimeout = 5 * time.Second
	}
	if s.IdleInTransactionTimeout == 0 {
		s.IdleInTransactionTimeout = 60 * time.Second
	}
	if s.ApplicationName == "" {
		s.ApplicationName = "maniflex"
	}
	if s.TimeZone == "" {
		s.TimeZone = "UTC"
	}
	return s
}

// ── Public API ────────────────────────────────────────────────────────────────

// Open opens a PostgreSQL connection and returns a maniflex.DBAdapter.
//
// writeDSN is the DSN for the primary (write) server.
// readDSN is the DSN for the replica (read) server. Pass an empty string to
// route reads to the same primary — useful when there is no replica.
//
// Both pools are configured with sensible production defaults:
//   - Connection pool sizes tuned for OLTP workloads.
//   - statement_timeout, lock_timeout, and idle_in_transaction_session_timeout
//     to prevent runaway queries and stalled transactions.
//   - application_name = "maniflex" visible in pg_stat_activity.
//   - TimeZone = UTC for consistent timestamp handling.
//
// Defaults can be overridden by calling OpenWithConfig.
//
//	// Single server (no replica):
//	db, err := postgres.Open("postgres://user:pass@primary/mydb", "", reg)
//
//	// Primary + read replica:
//	db, err := postgres.Open(
//	    "postgres://user:pass@primary/mydb",
//	    "postgres://user:pass@replica/mydb",
//	    reg,
//	)
func Open(writeDSN, readDSN string, reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
	return OpenWithConfig(writeDSN, readDSN, reg, PoolConfig{}, PoolConfig{}, SessionConfig{})
}

// OpenWithConfig is like Open but allows full control over connection pool and
// session parameters for both the write and read pools independently.
//
// Any zero-value fields in writePool, readPool, or session are replaced by
// the same production defaults that Open uses.
//
//	db, err := postgres.OpenWithConfig(
//	    writeDSN, readDSN, reg,
//	    postgres.PoolConfig{MaxOpenConns: 10},  // write pool
//	    postgres.PoolConfig{MaxOpenConns: 30},  // read pool
//	    postgres.SessionConfig{
//	        StatementTimeout: 10 * time.Second,
//	        ApplicationName:  "my-service",
//	    },
//	)
func OpenWithConfig(
	writeDSN, readDSN string,
	reg maniflex.RegistryAccessor,
	writePool, readPool PoolConfig,
	session SessionConfig,
) (maniflex.DBAdapter, error) {
	writePool = writePool.withDefaults(true)
	readPool = readPool.withDefaults(false)
	session = session.withDefaults()

	schemaName := session.schema()
	if !validSchemaIdent(schemaName) {
		return nil, fmt.Errorf("postgres: invalid schema name %q: must match %s",
			schemaName, schemaIdentPattern)
	}

	if readDSN == "" {
		readDSN = writeDSN
	}

	writeDB, err := openPool(writeDSN, writePool, session)
	if err != nil {
		return nil, fmt.Errorf("postgres: write pool: %w", err)
	}

	// Provision the search_path schema before any reads or AutoMigrate run.
	// SET search_path silently accepts a missing schema, so without this the
	// first CREATE TABLE fails with "no schema has been selected to create in"
	// (or lands in the wrong schema). ensureSchema is a no-op for "public".
	if err := ensureSchema(writeDB, schemaName); err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}

	// Always open a separate pool for reads, even when readDSN == writeDSN.
	// This matches the SQLite pattern (two sql.Open calls to the same file) and
	// avoids a double-close bug: sqlcore.Adapter.Close() closes writeDb and
	// readDb independently, so they must be distinct *sql.DB values.
	// Two separate pools to the same primary also lets us size them differently:
	// the write pool is kept small (serialised mutations), the read pool is larger
	// (concurrent reads scale well on Postgres).
	readDB, err := openPool(readDSN, readPool, session)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("postgres: read pool: %w", err)
	}

	adapter := sqlcore.New(writeDB, readDB, maniflex.Postgres, reg)
	adapter.SetErrorNormalizer(NormalizeError)
	return adapter, nil
}

// MustOpen is like Open but panics on error. Useful for package-level init.
func MustOpen(writeDSN, readDSN string, reg maniflex.RegistryAccessor) maniflex.DBAdapter {
	adapter, err := Open(writeDSN, readDSN, reg)
	if err != nil {
		panic(err)
	}
	return adapter
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// schemaIdentPattern restricts schema names to the conservative unquoted
// PostgreSQL identifier grammar. The name is embedded directly into
// SET search_path and CREATE SCHEMA (neither accepts a bind parameter), so we
// reject anything outside this grammar rather than attempt to quote-escape.
const schemaIdentPattern = `^[A-Za-z_][A-Za-z0-9_$]*$`

var schemaIdentRe = regexp.MustCompile(schemaIdentPattern)

func validSchemaIdent(s string) bool { return schemaIdentRe.MatchString(s) }

// ensureSchema verifies the search_path schema exists, creating it when absent.
//
// The public schema is present in every database and is left untouched (issuing
// CREATE SCHEMA on it can require privileges the application role may not hold).
// For any other schema we probe pg_catalog first and only run CREATE SCHEMA IF
// NOT EXISTS when it is missing, so a role with USAGE-but-not-CREATE on an
// already-provisioned schema still connects cleanly. Running against the write
// pool means the connection's SET search_path has already pointed at the
// (possibly missing) schema — harmless, since neither the catalog probe nor
// CREATE SCHEMA depends on search_path resolution.
func ensureSchema(db *sql.DB, schema string) error {
	if strings.EqualFold(schema, "public") {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`,
		schema).Scan(&exists); err != nil {
		return fmt.Errorf("check schema %q: %w", schema, err)
	}
	if exists {
		return nil
	}

	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		return fmt.Errorf("create schema %q: %w", schema, err)
	}
	return nil
}

// openPool opens a single *sql.DB with the given pool sizing and session
// settings, verifies the connection, and returns it.
func openPool(dsn string, pool PoolConfig, session SessionConfig) (*sql.DB, error) {
	// Use pq.NewConnector so we can wrap Connect() and inject session-level
	// SET commands on every new physical connection.
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DSN: %w", err)
	}

	db := sql.OpenDB(&sessionConnector{
		Connector: connector,
		session:   session,
	})

	// Pool sizing
	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)
	db.SetConnMaxIdleTime(pool.ConnMaxIdleTime)

	// Verify at least one connection can be established.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return db, nil
}

// sessionConnector wraps a pq driver.Connector to inject session-level GUC
// parameters (SET commands) on every new physical connection. This is
// equivalent to SQLite's PRAGMA calls: they must be re-applied per connection
// because Postgres does not persist session settings across reconnects.
type sessionConnector struct {
	driver.Connector
	session SessionConfig
}

func (c *sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}

	// Build the SET commands. We use a single multi-statement exec via the raw
	// driver connection so we avoid round-trips.
	setSQL := c.buildSetSQL()
	if setSQL == "" {
		return conn, nil
	}

	// driver.Execer is the standard interface for executing statements on a
	// raw driver connection. pq always implements it, so the fallback branch
	// below should never be reached in practice.
	if ex, ok := conn.(driver.Execer); ok {
		if _, err := ex.Exec(setSQL, nil); err != nil {
			conn.Close()
			return nil, fmt.Errorf("postgres: session init: %w", err)
		}
		return conn, nil
	}

	// Fallback: if the driver does not support Exec (non-pq drivers), close the
	// connection and surface a clear error rather than silently skipping session init.
	conn.Close()
	return nil, fmt.Errorf("postgres: driver does not implement driver.Execer; cannot apply session settings")
}

// buildSetSQL constructs a single SQL string containing all session SET
// commands derived from the SessionConfig.
func (c *sessionConnector) buildSetSQL() string {
	s := c.session
	sql := fmt.Sprintf(
		"SET TIME ZONE '%s'; SET application_name = '%s';",
		pgEscape(s.TimeZone),
		pgEscape(s.ApplicationName),
	)
	if s.StatementTimeout > 0 {
		sql += fmt.Sprintf(" SET statement_timeout = %d;", s.StatementTimeout.Milliseconds())
	}
	if s.LockTimeout > 0 {
		sql += fmt.Sprintf(" SET lock_timeout = %d;", s.LockTimeout.Milliseconds())
	}
	if s.IdleInTransactionTimeout > 0 {
		sql += fmt.Sprintf(" SET idle_in_transaction_session_timeout = %d;",
			s.IdleInTransactionTimeout.Milliseconds())
	}
	sql += fmt.Sprintf(" SET search_path TO %s;", s.schema())
	return sql
}

// pgEscape escapes a string value for safe embedding in a SET statement.
// Single-quotes are doubled; no other escaping is needed for GUC string values.
func pgEscape(s string) string {
	escaped := ""
	for _, ch := range s {
		if ch == '\'' {
			escaped += "''"
		} else {
			escaped += string(ch)
		}
	}
	return escaped
}
