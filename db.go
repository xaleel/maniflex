package maniflex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DriverType distinguishes SQL dialects used by DBAdapter implementations.
type DriverType int

const (
	Postgres DriverType = iota
	SQLite
)

// SQLTyper is implemented by Go types that need a driver-specific SQL column
// type instead of the default mapping in goTypeToSQL. The adapter calls
// SQLType before its built-in type switch, so any type implementing this
// interface controls its own schema column definition.
//
//	type Amount struct { ... }
//	func (Amount) SQLType(driver maniflex.DriverType) string {
//	    if driver == maniflex.Postgres { return "NUMERIC(19,4)" }
//	    return "TEXT"
//	}
type SQLTyper interface {
	SQLType(driver DriverType) string
}

// ErrNotFound is returned by adapter methods when a requested record does not exist.
var ErrNotFound = errors.New("record not found")

// ConstraintKind classifies a database constraint violation so the DB step can
// map it to the right HTTP status: unique / foreign-key → 409 Conflict, and
// not-null → 422 Validation. The zero value ("") is treated as a generic
// conflict, preserving the original behaviour for normalisers that don't set it.
type ConstraintKind string

const (
	ConstraintUnique     ConstraintKind = "unique"
	ConstraintForeignKey ConstraintKind = "foreign_key"
	ConstraintNotNull    ConstraintKind = "not_null"
)

// ErrConstraint is returned by adapter methods when a database constraint is
// violated. It is driver-neutral: SQLite "UNIQUE constraint failed:
// table.column" / "NOT NULL constraint failed: table.column" and Postgres error
// codes (23505, 23502, 23503) are normalised into this type before being
// returned to the DB step, which converts it to a 409 Conflict (unique/FK) or a
// 422 Validation error (not-null).
type ErrConstraint struct {
	// Kind classifies the violation (unique, foreign_key, not_null). Empty for
	// normalisers that predate the field; treated as a generic conflict.
	Kind ConstraintKind
	// Table is the DB table name where the violation occurred.
	Table string
	// Column is the column name that was violated, if the driver exposes it.
	// May be empty for drivers that do not provide column-level detail.
	Column string
	// Detail is the raw driver error message, for logging context.
	Detail string
}

func (e *ErrConstraint) Error() string {
	if e.Column != "" {
		return fmt.Sprintf("constraint violation on %s.%s: %s", e.Table, e.Column, e.Detail)
	}
	return fmt.Sprintf("constraint violation on %s: %s", e.Table, e.Detail)
}

// Row is the sanctioned shape for schemaless query results — raw SQL,
// aggregates, and recursive queries, whose columns aren't a registered model.
// It is an alias for map[string]any, so existing code keeps compiling; the named
// type documents intent and gives the dynamic escape hatch a single home as the
// typed-models migration removes ad-hoc maps elsewhere.
type Row = map[string]any

// ListResult is returned by DBAdapter.FindMany.
//
// As of the typed-models migration (Phase 3) Items holds the type-erased record
// carriers: each element is a *T for the queried model's GoType. The pipeline
// bridges them to maps during the transition; Phase 4 consumes them directly.
type ListResult struct {
	Items []any
	Total int64
	Query *QueryParams
}

// TxOptions mirrors sql.TxOptions so callers do not need to import database/sql.
type TxOptions = sql.TxOptions

// Tx is a transaction handle. It exposes the same write operations as
// DBAdapter so middleware can use it identically to the adapter itself.
//
// Obtain one via ctx.BeginTx() and store it on ctx.Tx:
//
//	tx, err := ctx.BeginTx(ctx.Ctx, nil)
//	if err != nil { ... }
//	ctx.Tx = tx
//	defer tx.Rollback() // no-op after Commit
//
// The default DB step routes through ctx.Tx when it is non-nil, so all
// Create / Update / Delete calls within the same request will use the transaction.
type Tx interface {
	// The same CRUD methods as DBAdapter, used when routing through a transaction.
	// Typed-models contract (Phase 3): the returned any is always a *T for
	// model.GoType; record passed to Create/Update is likewise a *T.
	FindByID(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (any, error)
	FindMany(ctx context.Context, model *ModelMeta, q *QueryParams) ([]any, int64, error)
	Create(ctx context.Context, model *ModelMeta, record any) (any, error)
	Update(ctx context.Context, model *ModelMeta, id string, record any, present map[string]struct{}) (any, error)
	Delete(ctx context.Context, model *ModelMeta, id string) error

	// FindByIDForUpdate fetches the record and acquires a pessimistic row-level
	// lock held until the transaction commits or rolls back.
	// Postgres: appends FOR UPDATE to the SELECT.
	// SQLite: behaves identically to FindByID — the lock is at the transaction
	// level (BEGIN IMMEDIATE / _txlock=immediate DSN option).
	FindByIDForUpdate(ctx context.Context, model *ModelMeta, id string) (any, error)

	// Commit finalises the transaction. Returns an error if the commit fails.
	Commit() error

	// Rollback aborts the transaction. Returns nil if the transaction has
	// already been committed (safe to defer unconditionally).
	Rollback() error
}

type RawResult interface {
	// When `Raw` called without select (as Exec), otherwise returns (nil, nil)
	LastInsertId() (*int64, error)
	// When `Raw` called without select (as Exec), otherwise returns (nil, nil)
	RowsAffected() (*int64, error)
	// Result of query if `Raw` called with select, otherwise returns (nil, nil)
	Rows() (*sql.Rows, error)
}

// rawableT is an optional interface satisfied by transaction implementations
// that support arbitrary SQL. Checked at runtime in ServerContext.RawQuery and
// ServerContext.RawExec to route raw statements through an active transaction.
// The unexported name keeps it out of the public API so the Tx interface
// contract remains stable.
type rawableT interface {
	RawQueryContext(ctx context.Context, query string, args ...any) ([]map[string]any, error)
	RawExecContext(ctx context.Context, query string, args ...any) (int64, error)
}

// DBAdapter is the interface all database backends must implement.
// Methods receive a *ModelMeta describing the target model and a *QueryParams
// carrying pagination, filters, sorts, and relation includes.
type DBAdapter interface {
	// AutoMigrate creates or updates tables/collections for the given models.
	// Called once on Server.Start() when Config.AutoMigrate is true.
	AutoMigrate(ctx context.Context, reg RegistryAccessor) error

	// BeginTx starts a database transaction with the given options.
	// Pass nil opts for the default isolation level.
	// The returned Tx must be committed or rolled back by the caller.
	BeginTx(ctx context.Context, opts *TxOptions) (Tx, error)

	// Run arbitrary SQL
	Raw(ctx context.Context, query string, args ...any) RawResult

	// Typed-models contract (Phase 3): every method below speaks the
	// type-erased record carrier. The returned any is ALWAYS a *T for
	// model.GoType (or nil with a non-nil error); the record passed to
	// Create/Update is likewise a *T. FindMany returns a []any of *T.
	// The pipeline bridges these to map[string]any during the transition
	// (see MapExec / recordToMap); Phase 4 consumes *T directly.

	// FindByID returns the record (*T) matching id.
	// Includes listed in q.Includes are populated as nested objects.
	// Returns ErrNotFound when the record does not exist.
	FindByID(ctx context.Context, model *ModelMeta, id string, q *QueryParams) (any, error)

	// FindByIDForUpdate fetches the record (*T) and acquires a pessimistic
	// row-level lock. Postgres: appends FOR UPDATE; use inside a transaction so
	// the lock is held until commit/rollback. SQLite: behaves like FindByID —
	// the lock is acquired at the transaction level (BEGIN IMMEDIATE).
	// Returns ErrNotFound when the record does not exist.
	FindByIDForUpdate(ctx context.Context, model *ModelMeta, id string) (any, error)

	// FindMany returns a page of records ([]any of *T) and the total count
	// before pagination.
	FindMany(ctx context.Context, model *ModelMeta, q *QueryParams) ([]any, int64, error)

	// Create inserts the record (*T). The adapter generates the ID if absent.
	// Returns the stored representation (*T) including generated fields.
	// Returns *ErrConstraint on unique/check violations.
	Create(ctx context.Context, model *ModelMeta, record any) (any, error)

	// Update applies a patch to the record identified by id. record is a *T
	// carrying the new values; present names the DB columns to write (PATCH
	// semantics) — only those (plus updated_at) are updated. Returns the updated
	// representation (*T). Returns ErrNotFound when absent, *ErrConstraint on
	// unique/check violations.
	Update(ctx context.Context, model *ModelMeta, id string, record any, present map[string]struct{}) (any, error)

	// Delete removes or soft-deletes the record identified by id.
	// Returns ErrNotFound when absent.
	Delete(ctx context.Context, model *ModelMeta, id string) error

	// Close releases any resources held by the adapter.
	Close() error
}
