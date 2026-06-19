// Package sqlcore provides a database/sql-backed DBAdapter that works with
// both PostgreSQL ($N placeholders) and SQLite (? placeholders).
package sqlcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xaleel/maniflex"

	"github.com/google/uuid"
)

// Adapter is a generic SQL DBAdapter backed by a *sql.DB.
type Adapter struct {
	writeDb       *sql.DB
	readDb        *sql.DB
	driver        maniflex.DriverType
	reg           maniflex.RegistryAccessor
	logger        *slog.Logger
	errNormalizer ErrorNormalizer

	// scanPlans caches the immutable column plan (kinds + struct indices) per
	// (model, column-set), keyed by a string signature. Built lazily on first
	// scan; safe for concurrent use. See scan.go.
	scanPlans sync.Map
}

// New wraps an existing *sql.DB and returns a maniflex.DBAdapter.
// Pass the same RegistryAccessor you gave to maniflex.New — the adapter
// uses it to resolve related models for include population and migrations.
func New(writeDb *sql.DB, readDb *sql.DB, driver maniflex.DriverType, reg maniflex.RegistryAccessor) *Adapter {
	return &Adapter{readDb: readDb, writeDb: writeDb, driver: driver, reg: reg}
}

// Ping verifies that both the write and read database connections are alive.
// It is called by the health handler when Config.HealthCheckDB is true.
// Ping implements the maniflex.Pinger interface.
func (a *Adapter) Ping(ctx context.Context) error {
	if err := a.writeDb.PingContext(ctx); err != nil {
		return fmt.Errorf("write db: %w", err)
	}
	// Only ping readDb separately when it is a distinct pool.
	// When both point to the same *sql.DB (no replica), one ping is enough.
	if a.readDb != a.writeDb {
		if err := a.readDb.PingContext(ctx); err != nil {
			return fmt.Errorf("read db: %w", err)
		}
	}
	return nil
}

// SetLogger sets the logger used for adapter-level output (AutoMigrate
// column-drift warnings, column additions). the Server framework calls this
// automatically before AutoMigrate when Config.Logger is set, so callers
// normally do not need to call it directly.
func (a *Adapter) SetLogger(l *slog.Logger) {
	a.logger = l
}

// WriteDB exposes the underlying writer *sql.DB. It is used by maniflex.Migrate
// to run versioned migrations inside a transaction. Application code should
// prefer the DBAdapter CRUD methods or Raw; this accessor exists so that
// cross-cutting helpers (migrations, custom maintenance scripts) can drive
// the same connection that the adapter uses for writes.
func (a *Adapter) WriteDB() *sql.DB {
	return a.writeDb
}

// DriverName returns "postgres" or "sqlite" so dialect-aware helpers (e.g.
// maniflex.Migrate) can choose the right placeholder syntax and column types
// without importing this package.
func (a *Adapter) DriverName() string {
	if a.driver == maniflex.Postgres {
		return "postgres"
	}
	return "sqlite"
}

// getLogger returns the configured logger, falling back to slog.Default().
func (a *Adapter) getLogger() *slog.Logger {
	if a.logger != nil {
		return a.logger
	}
	return slog.Default()
}

// Close closes the underlying *sql.DB.
func (a *Adapter) Close() error {
	if err := a.writeDb.Close(); err != nil {
		return err
	}
	return a.readDb.Close()
}

// ── AutoMigrate ───────────────────────────────────────────────────────────────

// AutoMigrate synchronises the database schema with the registered models.
//
// For each model it:
//  1. Creates the table if it does not exist (original behaviour, safe no-op on re-run).
//  2. Introspects the live table's columns and compares them to the model's fields.
//  3. Issues ALTER TABLE ADD COLUMN for every field present in the model but absent
//     from the table — allowing structs to grow without manual DDL.
//  4. Logs a WARNING (slog.LevelWarn) for every column present in the table but
//     absent from the model, indicating a likely removed field. It never drops
//     columns automatically.
//
// The entire migrate-and-diff sequence for each table runs inside a
// transaction (BeginTx → CREATE / introspect / ALTER … / CREATE INDEX → Commit)
// so it is atomic against concurrent startup processes: two replicas starting
// simultaneously each see either the pre-migration or post-migration schema,
// never a half-applied state.
func (a *Adapter) AutoMigrate(ctx context.Context, reg maniflex.RegistryAccessor) error {
	// Reject unmappable columns across every model before issuing any DDL, so a
	// field with no SQL mapping fails the whole migration loudly and atomically
	// instead of being silently dropped (see validateMappableColumns).
	for _, m := range reg.All() {
		if err := a.validateMappableColumns(m); err != nil {
			return fmt.Errorf("migrate %s: %w", m.Name, err)
		}
	}
	for _, m := range reg.All() {
		if err := a.migrateModel(ctx, m); err != nil {
			return fmt.Errorf("migrate %s: %w", m.Name, err)
		}
	}
	return nil
}

// validateMappableColumns rejects model fields whose Go type has no SQL column
// mapping (goTypeToSQL returns ""). Without this check such a field is silently
// dropped during migration — CREATE TABLE omits the column and ALTER TABLE ADD
// COLUMN no-ops — so the model registers and "migrates" successfully while the
// field has no backing column and writes to it never persist.
//
// A type is mappable if it is a scalar, time.Time, or implements
// maniflex.SQLTyper. Bare map / slice types (e.g. map[string]any, []string) are
// not: wrap them in a named type implementing maniflex.SQLTyper (SQLType +
// driver.Valuer + sql.Scanner), or exclude the field from persistence with the
// `mfx:"-"` tag. meta.Fields already excludes relations, has-many slices, and
// ignored fields, so every entry here is a genuine column candidate.
func (a *Adapter) validateMappableColumns(m *maniflex.ModelMeta) error {
	for _, f := range m.Fields {
		if a.goTypeToSQL(f.Type) == "" {
			return fmt.Errorf(
				"model %q field %q has Go type %s, which has no SQL column mapping; "+
					"wrap it in a named type implementing maniflex.SQLTyper, or exclude it "+
					"from persistence with the `mfx:\"-\"` tag",
				m.Name, f.Name, f.Type)
		}
	}
	return nil
}

// sqlExec is the subset of *sql.DB / *sql.Tx used by the migrate helpers.
// Threading an exec parameter through existingColumns / addColumn lets the
// transactional and non-transactional callers share one implementation.
type sqlExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (a *Adapter) migrateModel(ctx context.Context, m *maniflex.ModelMeta) error {
	tx, err := a.writeDb.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migrate tx for %s: %w", m.TableName, err)
	}
	defer tx.Rollback() //nolint:errcheck // committed on the happy path

	if err := a.migrateModelTx(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Adapter) migrateModelTx(ctx context.Context, exec sqlExec, m *maniflex.ModelMeta) error {
	// ── Step 1: build the expected column definitions ────────────────────────
	var cols []string
	for _, f := range m.Fields {
		def := a.columnDef(f)
		if def == "" {
			continue
		}
		cols = append(cols, "  "+def)
	}
	if len(cols) == 0 {
		return fmt.Errorf("model %s has no mappable fields", m.Name)
	}

	// Add companion HMAC columns for encrypted+unique fields inline in CREATE TABLE.
	for _, f := range m.Fields {
		if f.Tags.Encrypted && f.Tags.Unique {
			cols = append(cols, fmt.Sprintf("  %s TEXT NOT NULL DEFAULT '' UNIQUE",
				q(f.Tags.DBName+"_hmac")))
		}
	}

	// ── Step 2: CREATE TABLE IF NOT EXISTS ─────────────────────────────────
	// This is a no-op when the table already exists; it creates it on first run.
	createSQL := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (\n%s\n)",
		q(m.TableName),
		strings.Join(cols, ",\n"),
	)
	if _, err := exec.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create table %s: %w", m.TableName, err)
	}

	// ── Step 3: introspect existing columns ────────────────────────────────
	existingCols, err := a.existingColumns(ctx, exec, q(m.TableName))
	if err != nil {
		return fmt.Errorf("introspect %s: %w", m.TableName, err)
	}

	// ── Step 4: diff model fields vs live columns ──────────────────────────
	//
	// Build a set of model field DB column names (only mappable fields).
	modelCols := make(map[string]maniflex.FieldMeta, len(m.Fields))
	for _, f := range m.Fields {
		if a.goTypeToSQL(f.Type) == "" {
			continue // skip unmappable types
		}
		modelCols[f.Tags.DBName] = f
	}

	// Columns in the live DB but not in the model → warn (never drop).
	// Synthesized HMAC columns ({field}_hmac for encrypted+unique fields) are
	// managed by the framework and excluded from the warning.
	for col := range existingCols {
		if _, inModel := modelCols[col]; !inModel {
			if isHMACColumn(col, m) || isFTSColumn(col, m) {
				continue
			}
			a.getLogger().Warn("AutoMigrate: column exists in DB but not in model — "+
				"if the field was removed intentionally, drop the column manually",
				slog.String("table", m.TableName),
				slog.String("column", col),
				slog.String("model", m.Name),
			)
		}
	}

	// Columns in the model but not in the live DB → ALTER TABLE ADD COLUMN.
	for col, f := range modelCols {
		if existingCols[col] {
			continue
		}
		if err := a.addColumn(ctx, exec, m.TableName, f); err != nil {
			return fmt.Errorf("add column %s.%s: %w", m.TableName, col, err)
		}
		a.getLogger().Info("AutoMigrate: added column",
			slog.String("table", m.TableName),
			slog.String("column", col),
			slog.String("model", m.Name),
		)
	}

	// HMAC columns for encrypted+unique fields: add via ALTER TABLE if missing.
	for _, f := range m.Fields {
		if !f.Tags.Encrypted || !f.Tags.Unique {
			continue
		}
		hmacCol := f.Tags.DBName + "_hmac"
		if existingCols[hmacCol] {
			continue
		}
		var query string
		if a.driver == maniflex.Postgres {
			query = fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT NOT NULL DEFAULT ''",
				q(m.TableName), q(hmacCol))
		} else {
			query = fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN %s TEXT NOT NULL DEFAULT ''",
				q(m.TableName), q(hmacCol))
		}
		if _, err := exec.ExecContext(ctx, query); err != nil && !isDuplicateColumnError(err) {
			return fmt.Errorf("add hmac column %s.%s: %w", m.TableName, hmacCol, err)
		}
		// Add UNIQUE index separately (inline UNIQUE in ALTER ADD COLUMN is
		// not portable across all SQLite versions).
		idxName := "uidx_" + m.TableName + "_" + hmacCol
		idxQuery := fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s(%s)",
			idxName, q(m.TableName), q(hmacCol))
		if _, err := exec.ExecContext(ctx, idxQuery); err != nil {
			a.getLogger().Warn("AutoMigrate: could not create unique index for HMAC column",
				slog.String("table", m.TableName),
				slog.String("column", hmacCol),
				slog.String("error", err.Error()),
			)
		}
		a.getLogger().Info("AutoMigrate: added HMAC column",
			slog.String("table", m.TableName),
			slog.String("column", hmacCol),
			slog.String("model", m.Name),
		)
	}

	// ── Step 5: create extra indexes declared in meta.Indices ─────────────────
	for _, idx := range m.Indices {
		keyword := "INDEX"
		if idx.Unique {
			keyword = "UNIQUE INDEX"
		}
		cols := make([]string, len(idx.Columns))
		for i, c := range idx.Columns {
			// Columns may include direction hints like "version DESC" — quote only
			// the bare column name (first token), keep any suffix as-is.
			parts := strings.SplitN(c, " ", 2)
			if len(parts) == 2 {
				cols[i] = q(parts[0]) + " " + parts[1]
			} else {
				cols[i] = q(c)
			}
		}
		idxSQL := fmt.Sprintf(
			"CREATE %s IF NOT EXISTS %s ON %s (%s)",
			keyword, idx.Name, q(m.TableName), strings.Join(cols, ", "),
		)
		if _, err := exec.ExecContext(ctx, idxSQL); err != nil {
			a.getLogger().Warn("AutoMigrate: could not create index",
				slog.String("index", idx.Name),
				slog.String("table", m.TableName),
				slog.String("error", err.Error()),
			)
		}
	}

	// ── Step 6: provision the native full-text search index ───────────────────
	// (Postgres generated tsvector column + GIN, SQLite FTS5 shadow table).
	// No-op for models without mfx:"searchable" fields.
	if err := a.migrateFTS(ctx, exec, m); err != nil {
		return fmt.Errorf("fts %s: %w", m.TableName, err)
	}

	return nil
}

// isHMACColumn reports whether col is a framework-managed HMAC column
// ({field}_hmac where field is an encrypted+unique field on the model).
func isHMACColumn(col string, m *maniflex.ModelMeta) bool {
	if !strings.HasSuffix(col, "_hmac") {
		return false
	}
	base := strings.TrimSuffix(col, "_hmac")
	for _, f := range m.Fields {
		if f.Tags.DBName == base && f.Tags.Encrypted && f.Tags.Unique {
			return true
		}
	}
	return false
}

// existingColumns returns the set of column names currently present in the
// named table, using the appropriate dialect introspection query.
//
// The caller passes the same exec that issued the CREATE TABLE so the column
// list is observed in the same transaction snapshot. This eliminates a
// TOCTOU race two replicas would otherwise hit on parallel startup.
func (a *Adapter) existingColumns(ctx context.Context, exec sqlExec, table string) (map[string]bool, error) {
	cols := make(map[string]bool)
	switch a.driver {
	case maniflex.SQLite:
		// PRAGMA table_info does not accept a quoted identifier — use the raw name.
		rows, err := exec.QueryContext(ctx,
			fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name, colType string
			var notNull int
			var dfltValue sql.NullString
			var pk int
			if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
				return nil, err
			}
			cols[name] = true
		}
		return cols, rows.Err()

	case maniflex.Postgres:
		rows, err := exec.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns
			  WHERE table_schema = current_schema()
			    AND table_name   = $1`,
			table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			cols[name] = true
		}
		return cols, rows.Err()
	}
	return cols, nil
}

// addColumn issues an ALTER TABLE ADD COLUMN statement for the given field.
//
// NOT NULL columns without an explicit DEFAULT are given a zero-value default
// so the statement succeeds on tables that already have rows. Without this,
// both SQLite (≥ 3.37 strict mode) and Postgres reject the ALTER when existing
// rows cannot satisfy the NOT NULL constraint.
func (a *Adapter) addColumn(ctx context.Context, exec sqlExec, table string, f maniflex.FieldMeta) error {
	sqlType := a.goTypeToSQL(f.Type)
	if sqlType == "" {
		return nil // unmappable — skip silently
	}

	col := q(f.Tags.DBName) + " " + sqlType
	isPtr := f.Type.Kind() == reflect.Pointer
	if isPtr {
		col += " NULL"
	} else {
		col += " NOT NULL"
		// Build the DEFAULT clause: explicit mfx:"default:X" tag takes priority
		// (treated as a raw user value and quoted appropriately); otherwise we
		// synthesise the SQL zero-value literal for the Go type so that the ALTER
		// TABLE succeeds on a table that already has rows.
		if f.Tags.Default != "" {
			// User-supplied raw value → quote it for the SQL type.
			col += fmt.Sprintf(" DEFAULT %s", a.quotedDefault(f.Tags.Default, sqlType))
		} else if lit := a.zeroDefaultSQL(f.Type, sqlType); lit != "" {
			// Pre-formatted SQL literal (already quoted where needed) → embed directly.
			col += " DEFAULT " + lit
		}
	}

	if f.Tags.Unique && f.Tags.DBName != "id" {
		// UNIQUE constraints cannot be added via ALTER TABLE ADD COLUMN in some
		// dialects. We omit it here and note it in the log; callers can add an
		// index manually if uniqueness is required on an existing table.
		a.getLogger().Warn("AutoMigrate: UNIQUE constraint skipped on new column — "+
			"add a UNIQUE INDEX manually if required",
			slog.String("table", table),
			slog.String("column", f.Tags.DBName),
		)
	}

	// Postgres supports IF NOT EXISTS (avoiding a race if two instances start up
	// at the same time). SQLite does not, so we execute the ALTER and treat
	// "duplicate column name" as a safe no-op.
	var query string
	if a.driver == maniflex.Postgres {
		query = fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s", q(table), col)
	} else {
		query = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", q(table), col)
	}
	_, err := exec.ExecContext(ctx, query)
	if err != nil && isDuplicateColumnError(err) {
		return nil
	}
	return err
}

// isDuplicateColumnError reports whether err indicates that an ALTER TABLE
// ADD COLUMN failed because the column already exists.
// SQLite returns "duplicate column name: <col>".
// Postgres returns "column <col> of relation <table> already exists".
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists")
}

// zeroDefaultSQL returns a ready-to-embed SQL DEFAULT literal for the Go type
// when adding a NOT NULL column to an existing table. The returned string is
// already properly quoted for the target SQL dialect and can be concatenated
// directly into the ALTER TABLE statement.
//
// Returns "" for pointer (nullable) types and unsupported types — callers must
// check for the empty string and omit the DEFAULT clause in those cases.
func (a *Adapter) zeroDefaultSQL(t reflect.Type, sqlType string) string {
	if t.Kind() == reflect.Pointer {
		return ""
	}
	if t == reflect.TypeOf(time.Time{}) {
		if a.driver == maniflex.SQLite {
			return "'0001-01-01T00:00:00Z'"
		}
		return "'1970-01-01T00:00:00Z'"
	}
	switch t.Kind() {
	case reflect.String:
		return "''"
	case reflect.Bool:
		if a.driver == maniflex.Postgres {
			return "false"
		}
		return "0"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Float32, reflect.Float64:
		return "0"
	}
	return ""
}

func (a *Adapter) quotedDefault(val, sqlType string) string {
	switch sqlType {
	case "INTEGER", "BIGINT", "REAL", "BOOLEAN":
		return val
	case "TEXT", "TIMESTAMPTZ":
		escaped := strings.ReplaceAll(val, "'", "''")
		return fmt.Sprintf("'%s'", escaped)
	}
	return fmt.Sprintf("CAST('%s' AS %s)", val, sqlType)
}

func (a *Adapter) columnDef(f maniflex.FieldMeta) string {
	sqlType := a.goTypeToSQL(f.Type)
	if sqlType == "" {
		return ""
	}

	// Column name is quoted to handle reserved words (e.g. "order", "group").
	col := q(f.Tags.DBName) + " " + sqlType

	if f.Tags.DBName == "id" {
		col += " PRIMARY KEY"
	} else {
		if f.Type.Kind() == reflect.Pointer {
			col += " NULL"
		} else {
			col += " NOT NULL"
			if f.Tags.Default != "" {
				col += fmt.Sprintf(" DEFAULT %s", a.quotedDefault(f.Tags.Default, sqlType))
			} else if lit := a.zeroDefaultSQL(f.Type, sqlType); lit != "" && !f.Tags.Required {
				// 			    			don't default-zero required fields ^
				col += " DEFAULT " + lit
			}
		}
	}

	if f.Tags.Unique && f.Tags.DBName != "id" {
		col += " UNIQUE"
	}

	return col
}

// DriverType returns the SQL dialect used by this adapter.
// Implements the optional maniflex.adapterDriverTyper interface so that
// ServerContext.DriverType() can surface the dialect to packages (e.g.
// pkg/ledger) that need to build driver-specific parameterised queries.
func (a *Adapter) DriverType() maniflex.DriverType {
	return a.driver
}

var sqlTyperIface = reflect.TypeOf((*maniflex.SQLTyper)(nil)).Elem()

func (a *Adapter) goTypeToSQL(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(sqlTyperIface) {
		return reflect.Zero(t).Interface().(maniflex.SQLTyper).SQLType(a.driver)
	}
	if reflect.PointerTo(t).Implements(sqlTyperIface) {
		return reflect.New(t).Interface().(maniflex.SQLTyper).SQLType(a.driver)
	}
	if t == reflect.TypeOf(time.Time{}) {
		if a.driver == maniflex.Postgres {
			return "TIMESTAMPTZ"
		}
		return "TEXT"
	}
	switch t.Kind() {
	case reflect.String:
		return "TEXT"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return "INTEGER"
	case reflect.Int64:
		if a.driver == maniflex.Postgres {
			return "BIGINT"
		}
		return "INTEGER"
	case reflect.Float32, reflect.Float64:
		return "REAL"
	case reflect.Bool:
		if a.driver == maniflex.Postgres {
			return "BOOLEAN"
		}
		return "INTEGER" // SQLite: 0/1
	}
	return ""
}

// ── Placeholder builder ───────────────────────────────────────────────────────

type ph struct {
	driver maniflex.DriverType
	args   []any
}

func (p *ph) add(v any) string {
	p.args = append(p.args, v)
	if p.driver == maniflex.Postgres {
		return fmt.Sprintf("$%d", len(p.args))
	}
	return "?"
}

// PlaceholderBuilder is the exported version of the placeholder helper,
// allowing other packages (e.g. jobs/sql) to reuse this logic instead of
// reimplementing it locally.
//
// Usage:
//
//	pb := sqlcore.NewPlaceholderBuilder(driver)
//	query := fmt.Sprintf("SELECT * FROM users WHERE id = %s", pb.Add(userID))
//	rows, err := db.QueryContext(ctx, query, pb.Args()...)
type PlaceholderBuilder struct {
	driver maniflex.DriverType
	args   []any
}

// NewPlaceholderBuilder creates a PlaceholderBuilder for the given driver.
func NewPlaceholderBuilder(driver maniflex.DriverType) *PlaceholderBuilder {
	return &PlaceholderBuilder{driver: driver}
}

// Add appends a value to the argument list and returns the appropriate placeholder
// ($1, $2... for Postgres; ? for SQLite).
func (pb *PlaceholderBuilder) Add(v any) string {
	pb.args = append(pb.args, v)
	if pb.driver == maniflex.Postgres {
		return fmt.Sprintf("$%d", len(pb.args))
	}
	return "?"
}

// Args returns the collected arguments for the query.
func (pb *PlaceholderBuilder) Args() []any {
	return pb.args
}

// buildSelectCols returns the SELECT column list for a query. When fields is
// empty it falls back to "table.*". When non-empty it returns a comma-separated
// list of "table"."col" expressions, one per requested DB column name.
func buildSelectCols(tableName string, fields []string) string {
	if len(fields) == 0 {
		return q(tableName) + ".*"
	}
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = q(tableName) + "." + q(f)
	}
	return strings.Join(cols, ", ")
}

// ── FindByID ──────────────────────────────────────────────────────────────────

// FindByID returns the matching record as a field-populated *T scanned directly
// from the row (typed-models Phase 4). Includes are populated onto the record's
// extra carrier. createMap/updateMap still use findByIDMap for their map tails.
func (a *Adapter) FindByID(ctx context.Context, model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams) (any, error) {
	if model.GoType == nil {
		// Synthetic model (history, m2m junction): no struct to scan into — use
		// the map path; MapToRecord returns the map unchanged for GoType==nil.
		m, err := a.findByIDMap(ctx, model, id, qp)
		if err != nil {
			return nil, err
		}
		return maniflex.MapToRecord(model, m)
	}
	p := &ph{driver: a.driver}
	joinSQL := buildJoins(model, qp.Filters, qp.Sorts)

	var conditions []string
	if cond := softDeleteCond(model, model.TableName, a.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))
	if len(qp.Filters) > 0 {
		if extra := filterConds(model, qp.Filters, a.driver, p); extra != "" {
			conditions = append(conditions, extra)
		}
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s%s WHERE %s LIMIT 1",
		buildSelectCols(model.TableName, qp.Fields), q(model.TableName), joinSQL,
		strings.Join(conditions, " AND "),
	)
	rows, err := a.readDb.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("FindByID query: %w", err)
	}
	recs, err := a.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, maniflex.ErrNotFound
	}
	if err := populateIncludesTyped(ctx, a.readDb, a.reg, a.driver, model, recs, qp.Includes); err != nil {
		return nil, err
	}
	return recs[0], nil
}

func (a *Adapter) findByIDMap(ctx context.Context, model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams) (map[string]any, error) {
	p := &ph{driver: a.driver}

	joinSQL := buildJoins(model, qp.Filters, qp.Sorts)

	var conditions []string
	if cond := softDeleteCond(model, model.TableName, a.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))
	if len(qp.Filters) > 0 {
		if extra := filterConds(model, qp.Filters, a.driver, p); extra != "" {
			conditions = append(conditions, extra)
		}
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s%s WHERE %s LIMIT 1",
		buildSelectCols(model.TableName, qp.Fields), q(model.TableName), joinSQL,
		strings.Join(conditions, " AND "),
	)

	rows, err := a.readDb.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("FindByID query: %w", err)
	}
	defer rows.Close()

	results, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, maniflex.ErrNotFound
	}

	result := results[0]
	if err := populateIncludes(ctx, a.readDb, a.reg, a.driver, model, []map[string]any{result}, qp.Includes); err != nil {
		return nil, err
	}
	return result, nil
}

// ── FindByIDForUpdate ─────────────────────────────────────────────────────────

// FindByIDForUpdate fetches the row and acquires a pessimistic write lock.
// Postgres appends FOR UPDATE and routes through the write pool so the lock
// participates in an enclosing transaction. SQLite does a plain SELECT —
// the lock is at the transaction level (BEGIN IMMEDIATE / _txlock=immediate).
func (a *Adapter) FindByIDForUpdate(ctx context.Context, model *maniflex.ModelMeta, id string) (any, error) {
	p := &ph{driver: a.driver}

	var conditions []string
	if cond := softDeleteCond(model, model.TableName, a.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))

	query := fmt.Sprintf(
		"SELECT %s.* FROM %s WHERE %s LIMIT 1",
		q(model.TableName), q(model.TableName),
		strings.Join(conditions, " AND "),
	)
	if a.driver == maniflex.Postgres {
		query += " FOR UPDATE"
	}

	rows, err := a.writeDb.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("FindByIDForUpdate: %w", err)
	}
	if model.GoType == nil {
		results, err := scanRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, maniflex.ErrNotFound
		}
		return maniflex.MapToRecord(model, results[0])
	}
	recs, err := a.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, maniflex.ErrNotFound
	}
	return recs[0], nil
}

// ── FindMany ──────────────────────────────────────────────────────────────────

func (a *Adapter) FindMany(ctx context.Context, model *maniflex.ModelMeta, qp *maniflex.QueryParams) ([]any, int64, error) {
	if model.GoType == nil {
		results, total, err := a.findManyMap(ctx, model, qp)
		if err != nil {
			return nil, 0, err
		}
		recs := make([]any, len(results))
		for i, m := range results {
			rec, _ := maniflex.MapToRecord(model, m)
			recs[i] = rec
		}
		return recs, total, nil
	}

	joinSQL := buildJoins(model, qp.Filters, qp.Sorts) + ftsJoinSQL(model, qp, a.driver)

	// ─ count ─ (skipped in cursor mode — keyset pagination reports has_more.)
	total := int64(-1)
	if qp.Cursor == nil {
		cp := &ph{driver: a.driver}
		countConds := allWhereConds(model, qp.Filters, a.driver, cp)
		countConds = appendSearchCond(countConds, model, qp, a.driver, cp)
		countWhere := condToSQL(countConds)
		countQuery := fmt.Sprintf(
			"SELECT COUNT(DISTINCT %s.%s) FROM %s%s%s",
			q(model.TableName), q("id"), q(model.TableName), joinSQL, countWhere,
		)
		if err := a.readDb.QueryRowContext(ctx, countQuery, cp.args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count: %w", err)
		}
	}

	// ─ data ─
	dp := &ph{driver: a.driver}
	dataConds := allWhereConds(model, qp.Filters, a.driver, dp)
	dataConds = appendSearchCond(dataConds, model, qp, a.driver, dp)
	var orderSQL, limitSQL string
	if qp.Cursor != nil {
		dataConds, orderSQL, limitSQL = cursorDataClauses(model, qp.Cursor, qp.Limit, dataConds, dp)
	} else {
		orderSQL = listOrderSQL(model, qp, a.driver, dp)
		limitSQL = fmt.Sprintf(" LIMIT %s OFFSET %s", dp.add(qp.Limit), dp.add(qp.Offset()))
	}
	dataWhere := condToSQL(dataConds)

	dataQuery := fmt.Sprintf(
		"SELECT %s FROM %s%s%s%s%s",
		buildSelectCols(model.TableName, qp.Fields), q(model.TableName), joinSQL, dataWhere, orderSQL, limitSQL,
	)
	rows, err := a.readDb.QueryContext(ctx, dataQuery, dp.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("FindMany: %w", err)
	}
	recs, err := a.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if qp.Cursor != nil {
		recs = recs[:finalizeCursorPage(qp.Cursor, len(recs), qp.Limit, func(i int) (any, string) {
			m := maniflex.RecordToMap(model, recs[i])
			return m[qp.Cursor.Field], fmt.Sprint(m["id"])
		})]
	}
	if err := populateIncludesTyped(ctx, a.readDb, a.reg, a.driver, model, recs, qp.Includes); err != nil {
		return nil, 0, err
	}
	return recs, total, nil
}

// findManyMap is the scanRows/map FindMany, retained for GoType-less synthetic
// models that cannot be scanned into a struct.
func (a *Adapter) findManyMap(ctx context.Context, model *maniflex.ModelMeta, qp *maniflex.QueryParams) ([]map[string]any, int64, error) {
	joinSQL := buildJoins(model, qp.Filters, qp.Sorts) + ftsJoinSQL(model, qp, a.driver)

	total := int64(-1)
	if qp.Cursor == nil {
		cp := &ph{driver: a.driver}
		countConds := allWhereConds(model, qp.Filters, a.driver, cp)
		countConds = appendSearchCond(countConds, model, qp, a.driver, cp)
		countWhere := condToSQL(countConds)
		countQuery := fmt.Sprintf(
			"SELECT COUNT(DISTINCT %s.%s) FROM %s%s%s",
			q(model.TableName), q("id"), q(model.TableName), joinSQL, countWhere,
		)
		if err := a.readDb.QueryRowContext(ctx, countQuery, cp.args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count: %w", err)
		}
	}

	dp := &ph{driver: a.driver}
	dataConds := allWhereConds(model, qp.Filters, a.driver, dp)
	dataConds = appendSearchCond(dataConds, model, qp, a.driver, dp)
	var orderSQL, limitSQL string
	if qp.Cursor != nil {
		dataConds, orderSQL, limitSQL = cursorDataClauses(model, qp.Cursor, qp.Limit, dataConds, dp)
	} else {
		orderSQL = listOrderSQL(model, qp, a.driver, dp)
		limitSQL = fmt.Sprintf(" LIMIT %s OFFSET %s", dp.add(qp.Limit), dp.add(qp.Offset()))
	}
	dataWhere := condToSQL(dataConds)
	dataQuery := fmt.Sprintf(
		"SELECT %s FROM %s%s%s%s%s",
		buildSelectCols(model.TableName, qp.Fields), q(model.TableName), joinSQL, dataWhere, orderSQL, limitSQL,
	)
	rows, err := a.readDb.QueryContext(ctx, dataQuery, dp.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("FindMany: %w", err)
	}
	defer rows.Close()
	results, err := scanRows(rows)
	if err != nil {
		return nil, 0, err
	}
	if qp.Cursor != nil {
		results = results[:finalizeCursorPage(qp.Cursor, len(results), qp.Limit, func(i int) (any, string) {
			return results[i][qp.Cursor.Field], fmt.Sprint(results[i]["id"])
		})]
	}
	if err := populateIncludes(ctx, a.readDb, a.reg, a.driver, model, results, qp.Includes); err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

// populateIncludesTyped runs the existing map-based include population against a
// transient map view of the scanned records, then copies the populated relation
// keys onto each record's extra carrier (where marshalRecord reads them). For
// the common no-include path it does nothing.
func populateIncludesTyped(ctx context.Context, runner queryRunner, reg maniflex.RegistryAccessor, driver maniflex.DriverType, model *maniflex.ModelMeta, recs []any, includes []string) error {
	if len(includes) == 0 || len(recs) == 0 {
		return nil
	}
	maps := make([]map[string]any, len(recs))
	for i, r := range recs {
		maps[i] = maniflex.RecordToMap(model, r)
	}
	if err := populateIncludes(ctx, runner, reg, driver, model, maps, includes); err != nil {
		return err
	}
	for i, r := range recs {
		extra := make(map[string]any)
		for _, rel := range model.Relations {
			if v, ok := maps[i][rel.RelationKey]; ok {
				// Keep the map in extra (the response serializer reads it) AND
				// populate the typed companion/slice struct field for programmatic
				// access via the typed read helpers.
				extra[rel.RelationKey] = v
				setRelationField(reg, driver, r, rel, v)
			}
		}
		maniflex.SetExtra(r, extra)
	}
	return nil
}

// setRelationField populates a model's typed relation field (a companion *T for
// BelongsTo, or a []T / []*T slice for HasMany/ManyToMany) from the nested
// include data produced by populateIncludes. Best-effort: when the model has no
// matching field, or the related model isn't scannable, it leaves the field zero
// (the data still rides on the extra carrier for the response path).
func setRelationField(reg maniflex.RegistryAccessor, driver maniflex.DriverType, record any, rel maniflex.RelationMeta, nested any) {
	relMeta, ok := reg.Get(rel.RelatedModel)
	if !ok || relMeta.GoType == nil {
		return
	}
	fieldName := rel.FieldName
	if rel.Kind == maniflex.BelongsTo {
		fieldName = rel.CompanionField
	}
	if fieldName == "" {
		return
	}
	fv := reflect.ValueOf(record).Elem().FieldByName(fieldName)
	if !fv.IsValid() || !fv.CanSet() {
		return
	}

	if rel.Kind == maniflex.BelongsTo {
		m, ok := nested.(map[string]any)
		if !ok {
			return
		}
		rec, err := recordFromDBMap(relMeta, driver, m)
		if err != nil {
			return
		}
		setRelValue(fv, rec)
		return
	}

	// HasMany / ManyToMany → a slice field.
	rows := asMapSlice(nested)
	if rows == nil || fv.Kind() != reflect.Slice {
		return
	}
	slice := reflect.MakeSlice(fv.Type(), 0, len(rows))
	elemType := fv.Type().Elem()
	for _, m := range rows {
		rec, err := recordFromDBMap(relMeta, driver, m)
		if err != nil {
			continue
		}
		if ev := relElem(elemType, rec); ev.IsValid() {
			slice = reflect.Append(slice, ev)
		}
	}
	fv.Set(slice)
}

// setRelValue assigns a *RelatedModel into a companion field that may be either
// a pointer (*User) or a value (User).
func setRelValue(fv reflect.Value, rec any) {
	rv := reflect.ValueOf(rec) // *RelatedModel
	switch {
	case rv.Type().AssignableTo(fv.Type()):
		fv.Set(rv)
	case rv.Elem().Type().AssignableTo(fv.Type()):
		fv.Set(rv.Elem())
	}
}

// relElem coerces a *RelatedModel to a slice element type that may be a value
// (Comment) or a pointer (*Comment).
func relElem(elemType reflect.Type, rec any) reflect.Value {
	rv := reflect.ValueOf(rec) // *RelatedModel
	switch {
	case rv.Type().AssignableTo(elemType):
		return rv
	case rv.Elem().Type().AssignableTo(elemType):
		return rv.Elem()
	}
	return reflect.Value{}
}

// asMapSlice normalises an include's nested slice payload to []map[string]any,
// handling both []map[string]any and []any element forms.
func asMapSlice(v any) []map[string]any {
	switch s := v.(type) {
	case []map[string]any:
		return s
	case []any:
		out := make([]map[string]any, 0, len(s))
		for _, e := range s {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// ── Create ────────────────────────────────────────────────────────────────────

// Create inserts the record (*T) and returns the stored representation (*T).
// Transition (Phase 3): the record is bridged to its DB-column map, the existing
// map insert runs, and the result is wrapped back to a *T.
// recordData extracts the DB-column map from a record argument. The typed
// interface passes a *T (bridged via RecordToMap); for transition convenience it
// also accepts a raw map[string]any (used by some direct callers/tests).
func recordData(model *maniflex.ModelMeta, record any) map[string]any {
	if m, ok := record.(map[string]any); ok {
		return m
	}
	return maniflex.RecordToMap(model, record)
}

func (a *Adapter) Create(ctx context.Context, model *maniflex.ModelMeta, record any) (any, error) {
	// Typed write path (W4): when the record is a genuine struct carrying no extra
	// columns, build the INSERT straight from its fields. Records that carry extra
	// columns (encryption ciphertext companions like *_hmac, driver-shaped or NULL
	// values that didn't bind to a field) or GoType-less synthetic models fall back
	// to the map path so encryption stays map-side, per the migration plan.
	if ptr, ok := structForWrite(model, record); ok {
		query, args := a.buildInsert(model, ptr, maniflex.PresentColumns(record))
		m, err := a.execInsert(ctx, model, query, args, recordID(model, ptr))
		if err != nil {
			return nil, err
		}
		return maniflex.MapToRecord(model, m)
	}
	data := recordData(model, record)
	m, err := a.createMap(ctx, model, data)
	if err != nil {
		return nil, err
	}
	return maniflex.MapToRecord(model, m)
}

// structForWrite reports whether record is a non-nil struct pointer for a typed
// model (model.GoType set) that carries no extra columns, so a write can be built
// directly from its struct fields. A raw map, a GoType-less model, or a record
// with extra columns (encryption *_hmac, unbindable values) returns false and the
// caller uses the map write path.
func structForWrite(model *maniflex.ModelMeta, record any) (any, bool) {
	if model.GoType == nil {
		return nil, false
	}
	if _, isMap := record.(map[string]any); isMap {
		return nil, false
	}
	if len(maniflex.ExtraColumns(record)) > 0 {
		return nil, false
	}
	rv := reflect.ValueOf(record)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, false
	}
	return record, true
}

// recordID reads the string id field from a record struct (set by buildInsert
// when it generated one), used to refetch the row on drivers without RETURNING.
func recordID(model *maniflex.ModelMeta, ptr any) string {
	if f := model.FieldByDBName("id"); f != nil {
		v := reflect.ValueOf(ptr).Elem().FieldByIndex(f.Index)
		if v.Kind() == reflect.String {
			return v.String()
		}
	}
	return ""
}

// execInsert runs a prebuilt INSERT and returns the stored row as a DB-column
// map: RETURNING * on Postgres, or a refetch by id on drivers without it. Shared
// by the typed (buildInsert) and map (createMap) write paths.
func (a *Adapter) execInsert(ctx context.Context, model *maniflex.ModelMeta, query string, args []any, id string) (map[string]any, error) {
	if a.driver == maniflex.Postgres {
		rows, err := a.writeDb.QueryContext(ctx, query+" RETURNING *", args...)
		if err != nil {
			return nil, fmt.Errorf("create: %w", normalizeErr(a.errNormalizer, err, model.TableName))
		}
		defer rows.Close()
		results, err := scanRows(rows)
		if err != nil || len(results) == 0 {
			return nil, err
		}
		return results[0], nil
	}
	if _, err := a.writeDb.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("create: %w", normalizeErr(a.errNormalizer, err, model.TableName))
	}
	return a.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
}

func (a *Adapter) createMap(ctx context.Context, model *maniflex.ModelMeta, data map[string]any) (map[string]any, error) {
	// Generate an id when absent OR empty. The map pipeline strips id so it is
	// absent; the typed path (recordData of a *T) carries an empty string for an
	// unset id field — both must get a generated id.
	if s, _ := data["id"].(string); s == "" {
		data["id"] = uuid.New().String()
	}
	injectTimestamps(model, data, true)

	p := &ph{driver: a.driver}
	cols := make([]string, 0, len(data))
	phs := make([]string, 0, len(data))

	for col, val := range data {
		cols = append(cols, q(col))
		phs = append(phs, p.add(normalise(val)))
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		q(model.TableName),
		strings.Join(cols, ", "),
		strings.Join(phs, ", "),
	)
	return a.execInsert(ctx, model, query, p.args, fmt.Sprint(data["id"]))
}

// ── Update ────────────────────────────────────────────────────────────────────

// Update applies the present columns of the record (*T) and returns the updated
// representation (*T). Transition (Phase 3): the record is bridged to its
// present-column map, the existing map update runs, and the result is wrapped
// back to a *T.
func (a *Adapter) Update(ctx context.Context, model *maniflex.ModelMeta, id string, record any, present map[string]struct{}) (any, error) {
	// Typed write path (W4): build the UPDATE straight from the struct's present
	// columns when the record carries no extra columns (see structForWrite).
	if ptr, ok := structForWrite(model, record); ok {
		if len(present) == 0 {
			m, err := a.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
			if err != nil {
				return nil, err
			}
			return maniflex.MapToRecord(model, m)
		}
		query, args := a.buildUpdate(model, id, ptr, present)
		m, err := a.execUpdate(ctx, model, query, args, id)
		if err != nil {
			return nil, err
		}
		return maniflex.MapToRecord(model, m)
	}
	full := recordData(model, record)
	data := make(map[string]any, len(present))
	for col := range present {
		if v, ok := full[col]; ok {
			data[col] = v
		}
	}
	m, err := a.updateMap(ctx, model, id, data)
	if err != nil {
		return nil, err
	}
	return maniflex.MapToRecord(model, m)
}

// execUpdate runs a prebuilt UPDATE and returns the updated row as a DB-column
// map (RETURNING * on Postgres, refetch by id otherwise); zero rows affected →
// ErrNotFound. Shared by the typed (buildUpdate) and map (updateMap) write paths.
func (a *Adapter) execUpdate(ctx context.Context, model *maniflex.ModelMeta, query string, args []any, id string) (map[string]any, error) {
	if a.driver == maniflex.Postgres {
		rows, err := a.writeDb.QueryContext(ctx, query+" RETURNING *", args...)
		if err != nil {
			return nil, fmt.Errorf("update: %w", normalizeErr(a.errNormalizer, err, model.TableName))
		}
		defer rows.Close()
		results, err := scanRows(rows)
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, maniflex.ErrNotFound
		}
		return results[0], nil
	}
	res, err := a.writeDb.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update: %w", normalizeErr(a.errNormalizer, err, model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, maniflex.ErrNotFound
	}
	return a.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
}

func (a *Adapter) updateMap(ctx context.Context, model *maniflex.ModelMeta, id string, data map[string]any) (map[string]any, error) {
	if len(data) == 0 {
		return a.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
	}
	injectTimestamps(model, data, false)

	p := &ph{driver: a.driver}
	sets := make([]string, 0, len(data))
	for col, val := range data {
		sets = append(sets, fmt.Sprintf("%s = %s", q(col), p.add(normalise(val))))
	}

	where := fmt.Sprintf("%s = %s", q("id"), p.add(id))
	// Never update a soft-deleted row: scope the UPDATE so a deleted record
	// matches zero rows → ErrNotFound. Without this the Postgres RETURNING path
	// would mutate and return a soft-deleted row (the SQLite path only hid the
	// bug via its soft-delete-scoped FindByID afterwards).
	if c := softDeleteCond(model, model.TableName, a.driver); c != "" {
		where += " AND " + c
	}

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s",
		q(model.TableName),
		strings.Join(sets, ", "),
		where,
	)
	return a.execUpdate(ctx, model, query, p.args, id)
}

// ── Delete ────────────────────────────────────────────────────────────────────

func (a *Adapter) Delete(ctx context.Context, model *maniflex.ModelMeta, id string) error {
	if model.SoftDelete.Enabled {
		return a.softDelete(ctx, model, id)
	}
	p := &ph{driver: a.driver}
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", q(model.TableName), q("id"), p.add(id))
	res, err := a.writeDb.ExecContext(ctx, query, p.args...)
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return maniflex.ErrNotFound
	}
	return nil
}

func (a *Adapter) softDelete(ctx context.Context, model *maniflex.ModelMeta, id string) error {
	p := &ph{driver: a.driver}
	col := model.SoftDelete.Field
	var setExpr string
	var whereNotDelExpr string
	if model.SoftDelete.FieldType == maniflex.SoftDeleteBool {
		setExpr = fmt.Sprintf("%s = %s", q(col), p.add(true))
		whereNotDelExpr = fmt.Sprintf("NOT %s", q(col))
	} else {
		setExpr = fmt.Sprintf("%s = %s", q(col), p.add(time.Now().UTC()))
		whereNotDelExpr = fmt.Sprintf("%s IS NULL", q(col))
	}
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s AND %s",
		q(model.TableName), setExpr, q("id"), p.add(id), whereNotDelExpr)
	res, err := a.writeDb.ExecContext(ctx, query, p.args...)
	if err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return maniflex.ErrNotFound
	}
	return nil
}

// ── Query builders ────────────────────────────────────────────────────────────

// queryRunner is satisfied by both *sql.DB and *sql.Tx. Shared query
// builders (populateIncludes, fetchByIDs, ...) accept a runner so the
// non-tx and tx code paths reuse the same SELECT logic. Required for
// roadmap §11A.1: txAdapter reads must honour the same WHERE / JOIN /
// ORDER BY / include population path as the read-pool adapter.
type queryRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// buildJoins emits the LEFT JOINs needed by nested filters and nested sorts.
// A relation referenced by both a filter and a sort is joined only once; filter
// joins are emitted before sort-only joins for stable SQL.
func buildJoins(model *maniflex.ModelMeta, filters []*maniflex.FilterExpr, sorts []maniflex.SortExpr) string {
	seen := map[string]bool{}
	var joins []string
	addJoin := func(relKey, relTable, relFK string) {
		if seen[relKey] {
			return
		}
		seen[relKey] = true
		joins = append(joins, fmt.Sprintf(
			" LEFT JOIN %s AS %s ON %s.%s = %s.%s",
			q(relTable), q(relKey),
			q(model.TableName), q(relFK),
			q(relKey), q("id"),
		))
	}
	for _, f := range filters {
		if !f.IsNested {
			continue
		}
		addJoin(f.RelationKey, f.RelationTable, f.RelationFK)
	}
	for _, s := range sorts {
		if !s.IsNested {
			continue
		}
		addJoin(s.RelationKey, s.RelationTable, s.RelationFK)
	}
	return strings.Join(joins, "")
}

func allWhereConds(model *maniflex.ModelMeta, filters []*maniflex.FilterExpr, driver maniflex.DriverType, p *ph) []string {
	var conds []string
	if c := softDeleteCond(model, model.TableName, driver); c != "" {
		conds = append(conds, c)
	}
	conds = append(conds, filterConds(model, filters, driver, p))
	var out []string
	for _, c := range conds {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

func filterConds(model *maniflex.ModelMeta, filters []*maniflex.FilterExpr, driver maniflex.DriverType, p *ph) string {
	// Partition filters by group. Group <= 0 filters (including the FilterExpr
	// zero value) each become their own AND clause. Filters with the same
	// Group >= 1 are OR-ed together. The URL parser maps ?filter[N]= onto N+1,
	// so user-facing group 0 lands here as group 1.
	type groupEntry struct {
		exprs []*maniflex.FilterExpr
	}
	ungrouped := make([]*maniflex.FilterExpr, 0)
	grouped := make(map[int][]*maniflex.FilterExpr)
	var groupOrder []int
	seen := make(map[int]bool)

	for _, f := range filters {
		if f.Group <= 0 {
			ungrouped = append(ungrouped, f)
		} else {
			if !seen[f.Group] {
				seen[f.Group] = true
				groupOrder = append(groupOrder, f.Group)
			}
			grouped[f.Group] = append(grouped[f.Group], f)
		}
	}

	// Sort group keys for deterministic SQL.
	sort.Ints(groupOrder)

	buildCol := func(f *maniflex.FilterExpr) string {
		if f.IsLocale {
			col := q(model.TableName) + "." + q(f.Field)
			if driver == maniflex.Postgres {
				return col + "->>" + "'" + f.LocaleKey + "'"
			}
			return "json_extract(" + col + ", '$." + f.LocaleKey + "')"
		}
		if f.IsNested {
			return q(f.RelationKey) + "." + q(f.NestedField)
		}
		return q(model.TableName) + "." + q(f.Field)
	}

	var parts []string

	// Ungrouped filters — each is its own AND clause.
	for _, f := range ungrouped {
		parts = append(parts, buildCond(buildCol(f), f.Operator, f.Value, driver, p))
	}

	// Grouped filters — OR within a group, AND between groups.
	for _, gid := range groupOrder {
		exprs := grouped[gid]
		if len(exprs) == 1 {
			parts = append(parts, buildCond(buildCol(exprs[0]), exprs[0].Operator, exprs[0].Value, driver, p))
			continue
		}
		orParts := make([]string, len(exprs))
		for i, f := range exprs {
			orParts[i] = buildCond(buildCol(f), f.Operator, f.Value, driver, p)
		}
		parts = append(parts, "("+strings.Join(orParts, " OR ")+")")
	}

	return strings.Join(parts, " AND ")
}

func buildCond(col string, op maniflex.FilterOperator, val any, driver maniflex.DriverType, p *ph) string {
	switch op {
	case maniflex.OpEq:
		return fmt.Sprintf("%s = %s", col, p.add(val))
	case maniflex.OpNeq:
		return fmt.Sprintf("%s != %s", col, p.add(val))
	case maniflex.OpGt:
		return fmt.Sprintf("%s > %s", col, p.add(val))
	case maniflex.OpGte:
		return fmt.Sprintf("%s >= %s", col, p.add(val))
	case maniflex.OpLt:
		return fmt.Sprintf("%s < %s", col, p.add(val))
	case maniflex.OpLte:
		return fmt.Sprintf("%s <= %s", col, p.add(val))
	case maniflex.OpLike:
		return fmt.Sprintf("%s LIKE %s", col, p.add(val))
	case maniflex.OpILike:
		if driver == maniflex.Postgres {
			return fmt.Sprintf("%s ILIKE %s", col, p.add(val))
		}
		return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s)", col, p.add(val))
	case maniflex.OpIn:
		return inCond(col, val, p, false)
	case maniflex.OpNotIn:
		return inCond(col, val, p, true)
	case maniflex.OpIsNull:
		return fmt.Sprintf("%s IS NULL", col)
	case maniflex.OpNotNull:
		return fmt.Sprintf("%s IS NOT NULL", col)
	case maniflex.OpBetween:
		return betweenCond(col, val, p)
	}
	return "1=1"
}

// betweenCond expands a "lo,hi" value into "col >= lo AND col <= hi". The two
// bounds are validated to be present upstream in ParseFilterParam; a malformed
// value here degrades to a no-op rather than emitting broken SQL.
func betweenCond(col string, val any, p *ph) string {
	vals := maniflex.SplitCSV(fmt.Sprint(val))
	if len(vals) != 2 {
		return "1=1"
	}
	return fmt.Sprintf("(%s >= %s AND %s <= %s)", col, p.add(vals[0]), col, p.add(vals[1]))
}

func inCond(col string, val any, p *ph, negate bool) string {
	vals := maniflex.SplitCSV(fmt.Sprint(val))
	phs := make([]string, len(vals))
	for i, v := range vals {
		phs[i] = p.add(v)
	}
	op := "IN"
	if negate {
		op = "NOT IN"
	}
	return fmt.Sprintf("%s %s (%s)", col, op, strings.Join(phs, ", "))
}

func buildOrder(model *maniflex.ModelMeta, sorts []maniflex.SortExpr, driver maniflex.DriverType) string {
	if len(sorts) == 0 {
		return ""
	}
	parts := make([]string, len(sorts))
	for i, s := range sorts {
		dir := "ASC"
		if s.Direction == maniflex.SortDesc {
			dir = "DESC"
		}
		var col string
		switch {
		case s.IsLocale:
			base := q(model.TableName) + "." + q(s.DBName)
			if driver == maniflex.Postgres {
				col = base + "->>" + "'" + s.LocaleKey + "'"
			} else {
				col = "json_extract(" + base + ", '$." + s.LocaleKey + "')"
			}
		case s.IsNested:
			col = q(s.RelationKey) + "." + q(s.NestedField)
		default:
			col = q(model.TableName) + "." + q(s.DBName)
		}
		parts[i] = col + " " + dir
	}
	return " ORDER BY " + strings.Join(parts, ", ")
}

func softDeleteCond(model *maniflex.ModelMeta, tableAlias string, driver maniflex.DriverType) string {
	sd := model.SoftDelete
	if !sd.Enabled {
		return ""
	}
	col := q(tableAlias) + "." + q(sd.Field)
	if sd.FieldType == maniflex.SoftDeleteBool {
		if driver == maniflex.Postgres {
			return fmt.Sprintf("%s = FALSE", col)
		}
		return fmt.Sprintf("%s = 0", col)
	}
	return fmt.Sprintf("%s IS NULL", col)
}

// ── Include population ────────────────────────────────────────────────────────

func populateIncludes(ctx context.Context, runner queryRunner, reg maniflex.RegistryAccessor, driver maniflex.DriverType, model *maniflex.ModelMeta, rows []map[string]any, includes []string) error {
	if len(rows) == 0 || len(includes) == 0 || reg == nil {
		return nil
	}
	for _, key := range includes {
		rel := model.RelationByKey(key)
		if rel == nil {
			continue
		}
		relMeta, ok := reg.Get(rel.RelatedModel)
		if !ok {
			continue
		}
		switch rel.Kind {
		case maniflex.BelongsTo:
			if err := populateBelongsTo(ctx, runner, driver, relMeta, rel, rows); err != nil {
				return err
			}
		case maniflex.HasMany:
			if err := populateHasMany(ctx, runner, driver, relMeta, rel, rows); err != nil {
				return err
			}
		case maniflex.ManyToMany:
			if err := populateManyToMany(ctx, runner, driver, relMeta, rel, rows); err != nil {
				return err
			}
		}
	}
	return nil
}

func populateBelongsTo(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any) error {
	fkSet := map[string]bool{}
	for _, row := range rows {
		if v := row[rel.FKColumn]; v != nil {
			fkSet[fmt.Sprint(v)] = true
		}
	}
	if len(fkSet) == 0 {
		return nil
	}
	relRows, err := fetchByIDs(ctx, runner, driver, relMeta, fkSet)
	if err != nil {
		return fmt.Errorf("include %s: %w", rel.RelationKey, err)
	}
	byID := make(map[string]map[string]any, len(relRows))
	for _, r := range relRows {
		if id := fmt.Sprint(r["id"]); id != "" {
			byID[id] = r
		}
	}
	for _, row := range rows {
		if fk := fmt.Sprint(row[rel.FKColumn]); fk != "" {
			if related, ok := byID[fk]; ok {
				row[rel.RelationKey] = related
			}
		}
	}
	return nil
}

func populateHasMany(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any) error {
	parentIDs := map[string]bool{}
	for _, row := range rows {
		if id := fmt.Sprint(row["id"]); id != "" {
			parentIDs[id] = true
		}
	}
	if len(parentIDs) == 0 {
		return nil
	}

	p := &ph{driver: driver}
	ids := make([]string, 0, len(parentIDs))
	for id := range parentIDs {
		ids = append(ids, p.add(id))
	}

	var conditions []string
	if c := softDeleteCond(relMeta, relMeta.TableName, driver); c != "" {
		conditions = append(conditions, c)
	}
	conditions = append(conditions,
		fmt.Sprintf("%s.%s IN (%s)", q(relMeta.TableName), q(rel.FKColumn), strings.Join(ids, ", ")))

	query := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s",
		q(relMeta.TableName), strings.Join(conditions, " AND "),
	)
	relRows, err := runner.QueryContext(ctx, query, p.args...)
	if err != nil {
		return fmt.Errorf("include %s: %w", rel.RelationKey, err)
	}
	defer relRows.Close()

	children, err := scanRows(relRows)
	if err != nil {
		return err
	}

	grouped := make(map[string][]map[string]any)
	for _, c := range children {
		fk := fmt.Sprint(c[rel.FKColumn])
		grouped[fk] = append(grouped[fk], c)
	}
	for _, row := range rows {
		id := fmt.Sprint(row["id"])
		if kids, ok := grouped[id]; ok {
			row[rel.RelationKey] = kids
		} else {
			row[rel.RelationKey] = []map[string]any{}
		}
	}
	return nil
}

// populateManyToMany loads a many-to-many relation for the given parent rows.
// It performs a two-step join:
//  1. Query the junction table for all rows whose local FK is in the parent IDs.
//  2. Batch-fetch the related model rows by the collected remote IDs.
//
// Junction payload columns (everything except the two FK columns and "id") are
// surfaced as a "_through" sub-object on each included child.
func populateManyToMany(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any) error {
	if rel.ThroughTable == "" {
		return nil // unresolved stub — skip silently
	}

	// Step 1: query junction table
	parentIDSet := make(map[string]bool, len(rows))
	for _, row := range rows {
		if id := fmt.Sprint(row["id"]); id != "" {
			parentIDSet[id] = true
		}
	}
	if len(parentIDSet) == 0 {
		return nil
	}

	p := &ph{driver: driver}
	inList := make([]string, 0, len(parentIDSet))
	for id := range parentIDSet {
		inList = append(inList, p.add(id))
	}

	jQuery := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s IN (%s)",
		q(rel.ThroughTable),
		q(rel.ThroughLocalFK),
		strings.Join(inList, ", "),
	)
	jRows, err := runner.QueryContext(ctx, jQuery, p.args...)
	if err != nil {
		return fmt.Errorf("include %s (junction): %w", rel.RelationKey, err)
	}
	defer jRows.Close()
	junctions, err := scanRows(jRows)
	if err != nil {
		return fmt.Errorf("include %s (junction scan): %w", rel.RelationKey, err)
	}

	if len(junctions) == 0 {
		for _, row := range rows {
			row[rel.RelationKey] = []map[string]any{}
		}
		return nil
	}

	// Step 2: collect remote IDs and build junction payload index
	// junctionByRemote: remoteID → list of through payloads (one per parent that shares the same remote)
	type junctionEntry struct {
		parentID string
		through  map[string]any
	}
	remoteIDSet := make(map[string]bool)
	// parentID → []{ remoteID, through }
	type pair struct {
		remoteID string
		through  map[string]any
	}
	parentToRemotes := make(map[string][]pair)

	fkCols := map[string]bool{rel.ThroughLocalFK: true, rel.ThroughRemoteFK: true, "id": true}
	for _, jrow := range junctions {
		pid := fmt.Sprint(jrow[rel.ThroughLocalFK])
		rid := fmt.Sprint(jrow[rel.ThroughRemoteFK])
		if pid == "" || rid == "" {
			continue
		}
		remoteIDSet[rid] = true

		// Extract _through payload (everything except the FK and id columns)
		through := make(map[string]any)
		for k, v := range jrow {
			if !fkCols[k] {
				through[k] = v
			}
		}
		parentToRemotes[pid] = append(parentToRemotes[pid], pair{remoteID: rid, through: through})
	}

	// Step 3: batch-fetch related model rows
	relRows, err := fetchByIDs(ctx, runner, driver, relMeta, remoteIDSet)
	if err != nil {
		return fmt.Errorf("include %s (remote fetch): %w", rel.RelationKey, err)
	}
	relByID := make(map[string]map[string]any, len(relRows))
	for _, r := range relRows {
		if id := fmt.Sprint(r["id"]); id != "" {
			relByID[id] = r
		}
	}

	// Step 4: assemble results per parent row
	for _, row := range rows {
		pid := fmt.Sprint(row["id"])
		pairs := parentToRemotes[pid]
		children := make([]map[string]any, 0, len(pairs))
		for _, pr := range pairs {
			if relRow, ok := relByID[pr.remoteID]; ok {
				child := make(map[string]any, len(relRow)+1)
				for k, v := range relRow {
					child[k] = v
				}
				if len(pr.through) > 0 {
					child["_through"] = pr.through
				}
				children = append(children, child)
			}
		}
		row[rel.RelationKey] = children
	}
	return nil
}

func fetchByIDs(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, idSet map[string]bool) ([]map[string]any, error) {
	p := &ph{driver: driver}
	phs := make([]string, 0, len(idSet))
	for id := range idSet {
		phs = append(phs, p.add(id))
	}

	var conditions []string
	if c := softDeleteCond(relMeta, relMeta.TableName, driver); c != "" {
		conditions = append(conditions, c)
	}
	conditions = append(conditions,
		fmt.Sprintf("%s.%s IN (%s)", q(relMeta.TableName), q("id"), strings.Join(phs, ", ")))

	query := fmt.Sprintf(
		"SELECT * FROM %s WHERE %s",
		q(relMeta.TableName), strings.Join(conditions, " AND "),
	)
	rows, err := runner.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// ── Raw SQL ──────────────────────────────────────────────────────────────
type RawSqlResult struct {
	lastInsertIdVal *int64
	lastInsertIdErr error
	rowsAffectedVal *int64
	rowsAffectedErr error
	rowsVal         *sql.Rows
	rowsErr         error
}

func (r RawSqlResult) LastInsertId() (*int64, error) {
	return r.lastInsertIdVal, r.lastInsertIdErr
}
func (r RawSqlResult) RowsAffected() (*int64, error) {
	return r.rowsAffectedVal, r.rowsAffectedErr
}
func (r RawSqlResult) Rows() (*sql.Rows, error) {
	return r.rowsVal, r.rowsErr
}

func (a *Adapter) Raw(ctx context.Context, query string, args ...any) maniflex.RawResult {
	qLower := strings.ToLower(query)
	re := regexp.MustCompile(`\)\s*select`)
	isSelect := strings.HasPrefix(qLower, "select") ||
		(strings.HasPrefix(qLower, "with") && re.MatchString(qLower))
	if isSelect {
		rows, err := a.readDb.QueryContext(ctx, query, args...)
		return RawSqlResult{rowsVal: rows, rowsErr: err}
	}
	res, err := a.writeDb.ExecContext(ctx, query, args...)
	if res == nil {
		return RawSqlResult{lastInsertIdErr: err, rowsAffectedErr: err}
	}
	liv, lie := res.LastInsertId()
	rav, rae := res.RowsAffected()
	return RawSqlResult{
		lastInsertIdVal: &liv, lastInsertIdErr: lie,
		rowsAffectedVal: &rav, rowsAffectedErr: rae,
	}
}

// ── Row scanning ──────────────────────────────────────────────────────────────

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ── Misc helpers ──────────────────────────────────────────────────────────────

func normalise(v any) any {
	// Typed records carry pointer fields (*time.Time, *string, …); deref a
	// non-nil pointer so the value below is normalised, and map a nil pointer to
	// NULL. The map write path passes non-pointer values, so this is a no-op there.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		v = rv.Elem().Interface()
	}
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	// Locale maps (map[string]any or map[string]string) must be serialised to a
	// JSON string before being handed to database/sql, which has no native map
	// driver value type.
	switch mv := v.(type) {
	case maniflex.LocaleString:
		// A LocaleString flowing from a typed record field (Phase 4 write path)
		// arrives as the named type; serialise it to JSON like the raw maps below.
		if b, err := json.Marshal(mv); err == nil {
			return string(b)
		}
	case map[string]any:
		if b, err := json.Marshal(mv); err == nil {
			return string(b)
		}
	case map[string]string:
		if b, err := json.Marshal(mv); err == nil {
			return string(b)
		}
	}
	return v
}

func condToSQL(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// injectTimestamps sets created_at / updated_at when the model has those fields.
func injectTimestamps(model *maniflex.ModelMeta, dbMap map[string]any, isCreate bool) {
	now := time.Now().UTC()
	if isCreate {
		if f := model.FieldByDBName("created_at"); f != nil {
			dbMap["created_at"] = now
		}
	}
	if f := model.FieldByDBName("updated_at"); f != nil {
		dbMap["updated_at"] = now
	}
}
