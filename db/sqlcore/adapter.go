// Package sqlcore provides a database/sql-backed DBAdapter that works with
// both PostgreSQL ($N placeholders) and SQLite (? placeholders).
package sqlcore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xaleel/maniflex"

	"github.com/google/uuid"
)

var driverValuerIface = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

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
	// Postgres foreign keys are added after every table exists — a parent may be
	// registered after its child, and Postgres (unlike SQLite) requires the
	// referenced table to already exist. SQLite declared its FKs inline above.
	if err := a.migratePostgresForeignKeys(ctx, reg); err != nil {
		return err
	}
	return nil
}

// migratePostgresForeignKeys adds a FOREIGN KEY constraint for each
// database-enforced onDelete edge, once all tables exist. It is idempotent: a
// constraint already present is left alone, so re-running AutoMigrate does not
// fail. No-op on SQLite, which declared its FKs inline in CREATE TABLE.
func (a *Adapter) migratePostgresForeignKeys(ctx context.Context, reg maniflex.RegistryAccessor) error {
	if a.driver != maniflex.Postgres {
		return nil
	}
	for _, m := range reg.All() {
		for _, fk := range maniflex.ForeignKeysFor(reg, m) {
			exists, err := a.constraintExists(ctx, m.TableName, fk.Name)
			if err != nil {
				return fmt.Errorf("check fk %s on %s: %w", fk.Name, m.TableName, err)
			}
			if exists {
				continue
			}
			stmt := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
				q(m.TableName), q(fk.Name), fkConstraintClause(fk))
			if _, err := a.writeDb.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("add fk %s on %s: %w", fk.Name, m.TableName, err)
			}
		}
	}
	return nil
}

// constraintExists reports whether a named constraint is already defined on a
// table (Postgres information_schema).
func (a *Adapter) constraintExists(ctx context.Context, table, name string) (bool, error) {
	var n int
	err := a.writeDb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_name = $1 AND constraint_name = $2`,
		table, name).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// fkConstraintClause renders a ForeignKeySpec as a table-level FOREIGN KEY clause,
// for use inside CREATE TABLE (SQLite) or after ADD CONSTRAINT (Postgres).
func fkConstraintClause(fk maniflex.ForeignKeySpec) string {
	return fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s)%s",
		q(fk.Column), q(fk.RefTable), q(fk.RefColumn), onDeleteClause(fk.OnDelete))
}

// onDeleteClause renders the ON DELETE action, or "" for none.
func onDeleteClause(action maniflex.OnDeleteAction) string {
	switch action {
	case maniflex.OnDeleteCascade:
		return " ON DELETE CASCADE"
	case maniflex.OnDeleteSetNull:
		return " ON DELETE SET NULL"
	case maniflex.OnDeleteRestrict:
		return " ON DELETE RESTRICT"
	default:
		return ""
	}
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

	// Foreign-key constraints for database-enforced onDelete edges (5.16). SQLite
	// declares them inline in CREATE TABLE — it can only add a FK at create time,
	// but it also permits forward references, so registration order does not
	// matter and foreign_keys(ON) (set on every connection) enforces them. Postgres
	// requires the parent table to exist, so its FKs are added by a second pass in
	// AutoMigrate after every table is created.
	if a.driver == maniflex.SQLite {
		for _, fk := range maniflex.ForeignKeysFor(a.reg, m) {
			cols = append(cols, "  "+fkConstraintClause(fk))
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
	// A column that is present but whose type has since changed can't be fixed
	// here (AutoMigrate never rewrites a column) — warn so the drift is visible.
	for col, f := range modelCols {
		if liveType, exists := existingCols[col]; exists {
			a.warnTypeDrift(m, f, liveType)
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
		if _, exists := existingCols[hmacCol]; !exists {
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
			a.getLogger().Info("AutoMigrate: added HMAC column",
				slog.String("table", m.TableName),
				slog.String("column", hmacCol),
				slog.String("model", m.Name),
			)
		}

		// The UNIQUE index is created separately (inline UNIQUE in ALTER ADD
		// COLUMN is not portable across all SQLite versions), and outside the
		// "column is missing" branch above deliberately: it used to sit inside,
		// so a boot that added the column but failed to build the index never
		// retried — every later boot saw the column, skipped the block, and left
		// the constraint permanently absent. CREATE ... IF NOT EXISTS makes the
		// retry free.
		idxName := "uidx_" + m.TableName + "_" + hmacCol
		idxQuery := fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s(%s)",
			idxName, q(m.TableName), q(hmacCol))
		if _, err := exec.ExecContext(ctx, idxQuery); err != nil {
			return uniqueIndexFailure(m.TableName, idxName, []string{hmacCol}, err)
		}
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
			// A failed UNIQUE index is a broken invariant, not a slow query: the
			// model declares the constraint and the database is not enforcing it,
			// so duplicates would be accepted silently from here on. A plain
			// index costs a table scan, which is a performance problem the
			// application still survives — so only the unique case is fatal.
			if idx.Unique {
				return uniqueIndexFailure(m.TableName, idx.Name, idx.Columns, err)
			}
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

// existingColumns returns the columns currently present in the named table,
// mapped to the type the database reports for each, using the appropriate
// dialect introspection query.
//
// The caller passes the same exec that issued the CREATE TABLE so the column
// list is observed in the same transaction snapshot. This eliminates a
// TOCTOU race two replicas would otherwise hit on parallel startup.
func (a *Adapter) existingColumns(ctx context.Context, exec sqlExec, table string) (map[string]string, error) {
	cols := make(map[string]string)
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
			cols[name] = colType
		}
		return cols, rows.Err()

	case maniflex.Postgres:
		rows, err := exec.QueryContext(ctx,
			`SELECT column_name, data_type FROM information_schema.columns
			  WHERE table_schema = current_schema()
			    AND table_name   = $1`,
			table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name, dataType string
			if err := rows.Scan(&name, &dataType); err != nil {
				return nil, err
			}
			cols[name] = dataType
		}
		return cols, rows.Err()
	}
	return cols, nil
}

// warnTypeDrift reports a live column whose type no longer matches the model's.
//
// AutoMigrate only ever ADDs columns — it never rewrites one — so changing a
// field's Go type (int → string) leaves the old column in place and the drift
// stays invisible until a scan or an insert fails at runtime. Rewriting the
// column is not something a migration on startup should do unasked (it can lose
// data, and on a large table it locks), so this warns and leaves the schema
// alone; the change belongs in an explicit versioned migration.
//
// The comparison is deliberately coarse. It maps both types to a storage family
// and speaks up only when both are recognised and differ, because every driver
// spells its types its own way — Postgres reports TIMESTAMPTZ as "timestamp with
// time zone" and NUMERIC(20,4) as "numeric" — and a warning that cries wolf on a
// correctly-migrated table is worse than no warning at all.
func (a *Adapter) warnTypeDrift(m *maniflex.ModelMeta, f maniflex.FieldMeta, liveType string) {
	modelType := a.goTypeToSQL(f.Type)
	want, live := sqlTypeFamily(modelType), sqlTypeFamily(liveType)
	if want == "" || live == "" || want == live {
		return
	}
	a.getLogger().Warn("AutoMigrate: column type differs from the model — AutoMigrate never "+
		"rewrites a column, so the database keeps the old type and reads or writes of this "+
		"field may fail; change it with an explicit versioned migration",
		slog.String("table", m.TableName),
		slog.String("column", f.Tags.DBName),
		slog.String("model", m.Name),
		slog.String("db_type", liveType),
		slog.String("model_type", modelType),
	)
}

// sqlTypeFamily reduces a SQL type — as declared by a model or as reported by
// the database — to the storage class it belongs to, so the two can be compared
// across dialects and spellings. It returns "" for a type it doesn't recognise,
// which callers read as "no opinion".
func sqlTypeFamily(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 { // NUMERIC(20,4) → numeric
		t = strings.TrimSpace(t[:i])
	}
	switch {
	case t == "":
		return ""
	case strings.Contains(t, "json"):
		return "json"
	case strings.Contains(t, "bool"):
		return "boolean"
	case strings.Contains(t, "int"), strings.Contains(t, "serial"):
		return "integer"
	case strings.Contains(t, "text"), strings.Contains(t, "char"), strings.Contains(t, "clob"):
		return "text"
	case strings.Contains(t, "numeric"), strings.Contains(t, "decimal"), strings.Contains(t, "money"):
		return "numeric"
	case strings.Contains(t, "real"), strings.Contains(t, "float"), strings.Contains(t, "double"):
		return "real"
	case strings.Contains(t, "timestamp"), strings.Contains(t, "date"), strings.Contains(t, "time"):
		return "timestamp"
	case strings.Contains(t, "blob"), strings.Contains(t, "bytea"), strings.Contains(t, "binary"):
		return "blob"
	case strings.Contains(t, "uuid"):
		return "uuid"
	}
	return ""
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
	}
	// Build the DEFAULT clause: an explicit mfx:"default:X" tag takes priority
	// (treated as a raw user value and quoted appropriately); otherwise we
	// synthesise the SQL zero-value literal for the Go type so that the ALTER
	// TABLE succeeds on a table that already has rows.
	//
	// The explicit tag applies to nullable columns too. It used to sit inside
	// the NOT NULL branch, so mfx:"default:7" on a *int was discarded without a
	// word — found while fixing audit MS-13, which relies on a pointer field
	// being the way to store an explicit zero in a defaulted column. The
	// synthesised zero literal stays NOT NULL-only: it exists solely so the
	// ALTER succeeds against existing rows, which a NULL column does not need,
	// and defaulting a nullable column to zero would be a claim the model never
	// made.
	if f.Tags.Default != "" {
		// User-supplied raw value → quote it for the SQL type.
		col += fmt.Sprintf(" DEFAULT %s", a.quotedDefault(f.Tags.Default, sqlType))
	} else if lit := a.zeroDefaultSQL(f.Type, sqlType); lit != "" && !isPtr {
		// Pre-formatted SQL literal (already quoted where needed) → embed directly.
		col += " DEFAULT " + lit
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
	if _, err := exec.ExecContext(ctx, query); err != nil && !isDuplicateColumnError(err) {
		return err
	}

	// An inline UNIQUE cannot be added via ALTER TABLE ADD COLUMN portably, so we
	// add it as a separate UNIQUE INDEX (mirroring the create-table + HMAC paths),
	// making create and alter equivalent. If existing rows already violate the
	// constraint the index build fails — we surface that as a migration error
	// (naming the table/column) rather than silently dropping a constraint the
	// caller's correctness may depend on.
	if f.Tags.Unique && f.Tags.DBName != "id" {
		idxName := "uidx_" + table + "_" + f.Tags.DBName
		idxQuery := fmt.Sprintf(
			"CREATE UNIQUE INDEX IF NOT EXISTS %s ON %s(%s)",
			idxName, q(table), q(f.Tags.DBName))
		if _, err := exec.ExecContext(ctx, idxQuery); err != nil {
			return fmt.Errorf(
				"AutoMigrate: cannot add UNIQUE constraint on new column %s.%s "+
					"(existing rows may already violate it): %w",
				table, f.Tags.DBName, err)
		}
	}
	return nil
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
	// SQLTyper container/custom types (LocaleString, JSON maps/arrays, money.Amount,
	// …) have no scalar zero default, so omitting them on insert otherwise violates
	// NOT NULL. Synthesise their empty stored form so an absent JSON/locale/custom
	// column inserts its empty container instead of failing.
	if t.Implements(sqlTyperIface) || reflect.PointerTo(t).Implements(sqlTyperIface) {
		return a.sqlTyperZeroDefault(t, sqlType)
	}
	return ""
}

// sqlTyperZeroDefault returns the empty stored form of a SQLTyper type as an
// already-quoted SQL DEFAULT literal, or "" when it can't be determined (the
// caller then omits the DEFAULT clause). It prefers the type's own driver.Valuer
// zero value (e.g. money.Amount → "0.0000", a JSON type whose Value() emits
// "{}"/"[]"); failing that it falls back by container kind for JSON-backed types
// the adapter encodes itself, such as maniflex.LocaleString (a map with no
// Valuer).
func (a *Adapter) sqlTyperZeroDefault(t reflect.Type, sqlType string) string {
	if lit, ok := zeroValuerLiteral(t); ok {
		return a.quotedDefault(lit, sqlType)
	}
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	switch base.Kind() {
	case reflect.Map:
		return a.quotedDefault("{}", sqlType)
	case reflect.Slice:
		return a.quotedDefault("[]", sqlType)
	}
	return ""
}

// zeroValuerLiteral returns the string form of a type's zero-value driver.Valuer
// output, if the type (or its pointer) implements driver.Valuer and the zero
// value yields a non-nil, error-free value.
func zeroValuerLiteral(t reflect.Type) (string, bool) {
	var zv reflect.Value
	switch {
	case t.Implements(driverValuerIface):
		zv = reflect.Zero(t)
	case reflect.PointerTo(t).Implements(driverValuerIface):
		zv = reflect.New(t) // *t pointing at a zero t
	default:
		return "", false
	}
	val, err := zv.Interface().(driver.Valuer).Value()
	if err != nil || val == nil {
		return "", false
	}
	switch s := val.(type) {
	case string:
		return s, true
	case []byte:
		return string(s), true
	default:
		return fmt.Sprint(val), true
	}
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
		isPtr := f.Type.Kind() == reflect.Pointer
		if isPtr {
			col += " NULL"
		} else {
			col += " NOT NULL"
		}
		// Mirrors addColumn: an explicit default: tag applies whether or not the
		// column is nullable, while the synthesised zero literal is NOT NULL-only.
		// See the note there — the tag used to be dropped for pointer fields.
		if f.Tags.Default != "" {
			col += fmt.Sprintf(" DEFAULT %s", a.quotedDefault(f.Tags.Default, sqlType))
		} else if lit := a.zeroDefaultSQL(f.Type, sqlType); lit != "" && !f.Tags.Required && !isPtr {
			// 			    			don't default-zero required fields ^
			col += " DEFAULT " + lit
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
		return echoRecord(model, m)
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
	if err := populateIncludesTyped(ctx, a.readDb, a.reg, a.driver, model, recs, qp); err != nil {
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
	if err := populateIncludes(ctx, a.readDb, a.reg, a.driver, model, []map[string]any{result}, qp); err != nil {
		return nil, err
	}
	return result, nil
}

// ── FindByIDForUpdate ─────────────────────────────────────────────────────────

// FindByIDForUpdate fetches the row and acquires a pessimistic write lock.
// Postgres appends FOR UPDATE and routes through the write pool so the lock
// participates in an enclosing transaction. SQLite does a plain SELECT —
// the lock is at the transaction level, taken at BEGIN (db/sqlite opens write
// connections with _txlock=immediate).
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

// countQuerySQL builds the total-count query for an offset list request. It uses
// COUNT(*), not COUNT(DISTINCT id): every join a list query can carry is 1:1, so no
// join fans the base table out and a DISTINCT would only force a needless sort/hash
// over the whole filtered set (PERF-1). Nested filters and sorts join a BelongsTo
// relation on its primary key (any other kind is rejected at parse time), and the
// SQLite FTS join matches one shadow row per base row on rowid. HasMany /
// ManyToMany includes are loaded in separate queries and never joined here. If a
// fan-out join is ever added to this query, the count must go back to COUNT(DISTINCT
// <base>.id) — COUNT(*) would then count joined rows, not distinct base rows.
func countQuerySQL(table, joinSQL, whereSQL string) string {
	return fmt.Sprintf("SELECT COUNT(*) FROM %s%s%s", q(table), joinSQL, whereSQL)
}

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
		countQuery := countQuerySQL(model.TableName, joinSQL, countWhere)
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
	if err := populateIncludesTyped(ctx, a.readDb, a.reg, a.driver, model, recs, qp); err != nil {
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
		countQuery := countQuerySQL(model.TableName, joinSQL, countWhere)
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
	if err := populateIncludes(ctx, a.readDb, a.reg, a.driver, model, results, qp); err != nil {
		return nil, 0, err
	}
	return results, total, nil
}

// populateIncludesTyped runs the existing map-based include population against a
// transient map view of the scanned records, then copies the populated relation
// keys onto each record's extra carrier (where marshalRecord reads them). For
// the common no-include path it does nothing.
func populateIncludesTyped(ctx context.Context, runner queryRunner, reg maniflex.RegistryAccessor, driver maniflex.DriverType, model *maniflex.ModelMeta, recs []any, qp *maniflex.QueryParams) error {
	if qp == nil || len(qp.Includes) == 0 || len(recs) == 0 {
		return nil
	}
	maps := make([]map[string]any, len(recs))
	for i, r := range recs {
		maps[i] = maniflex.RecordToMap(model, r)
	}
	if err := populateIncludes(ctx, runner, reg, driver, model, maps, qp); err != nil {
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
		return echoRecord(model, m)
	}
	data := recordData(model, record)
	m, err := a.createMap(ctx, model, data)
	if err != nil {
		return nil, err
	}
	return echoRecord(model, m)
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
			return echoRecord(model, m)
		}
		query, args := a.buildUpdate(model, id, ptr, present)
		m, err := a.execUpdate(ctx, model, query, args, id)
		if err != nil {
			return nil, err
		}
		return echoRecord(model, m)
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
	return echoRecord(model, m)
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

// Restore implements maniflex.Restorer: it clears the soft-delete marker on one
// row and returns the restored record (5.19).
//
// It has to be its own statement rather than an Update, because every read and
// update path applies softDeleteCond — the row it targets is invisible to all of
// them. It is the mirror image of softDelete, down to the guard: softDelete
// requires the row to be live, this requires it to be deleted, so restoring a
// row that was never deleted is a 404 rather than a silent success.
//
// Only the marker is written. updated_at is deliberately left alone so a restore
// does not read as an edit to anything watching that column.
func (a *Adapter) Restore(ctx context.Context, model *maniflex.ModelMeta, id string,
	qp *maniflex.QueryParams,
) (any, error) {
	query, args, err := restoreStmt(model, id, qp, a.driver)
	if err != nil {
		return nil, err
	}
	res, err := a.writeDb.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("restore: %w", normalizeErr(a.errNormalizer, err, model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, maniflex.ErrNotFound
	}
	// The marker is cleared, so the row is visible to the ordinary read path
	// now. The read carries no filters of its own: the scope was already
	// enforced by the UPDATE above, which would have matched no row otherwise.
	// (FindByID dereferences its QueryParams, so it gets an empty one, not nil.)
	return a.FindByID(ctx, model, id, &maniflex.QueryParams{})
}

// restoreStmt builds the UPDATE that clears a soft-delete marker, together with
// its arguments. Shared by the adapter and the transaction so the two cannot
// drift — the divergence that let a soft delete behave differently inside a
// transaction is exactly the bug this avoids repeating.
func restoreStmt(model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams,
	driver maniflex.DriverType,
) (string, []any, error) {
	if !model.SoftDelete.Enabled {
		// A hard-delete model has no marker to clear and no deleted rows to find.
		return "", nil, maniflex.ErrNotFound
	}
	p := &ph{driver: driver}
	col := model.SoftDelete.Field

	var setExpr, whereDeletedExpr string
	if model.SoftDelete.FieldType == maniflex.SoftDeleteBool {
		setExpr = fmt.Sprintf("%s = %s", q(col), p.add(false))
		whereDeletedExpr = q(col) // the marker is set
	} else {
		setExpr = fmt.Sprintf("%s = NULL", q(col))
		whereDeletedExpr = fmt.Sprintf("%s IS NOT NULL", q(col))
	}

	conds := []string{
		fmt.Sprintf("%s = %s", q("id"), p.add(id)),
		whereDeletedExpr,
	}
	if scope := restoreScopeCond(model, qp, driver, p); scope != "" {
		conds = append(conds, scope)
	}

	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		q(model.TableName), setExpr, strings.Join(conds, " AND ")), p.args, nil
}

// restoreScopeCond renders the request's forced filters as a subquery the UPDATE
// can require membership of, so a caller cannot restore a row outside their
// tenancy or force-filter scope by knowing its id.
//
// A subquery rather than an inline predicate because filterConds qualifies every
// column with the table name and may emit joins for a relation-based scope
// (db.ForceFilterVia); neither belongs in an UPDATE's own WHERE, and both are
// perfectly at home in a SELECT. Note the subquery deliberately omits
// softDeleteCond — the row it must find is the deleted one.
func restoreScopeCond(model *maniflex.ModelMeta, qp *maniflex.QueryParams,
	driver maniflex.DriverType, p *ph,
) string {
	if qp == nil || len(qp.Filters) == 0 {
		return ""
	}
	conds := filterConds(model, qp.Filters, driver, p)
	if conds == "" {
		return ""
	}
	joinSQL := buildJoins(model, qp.Filters, nil)
	return fmt.Sprintf("%s IN (SELECT %s.%s FROM %s%s WHERE %s)",
		q("id"), q(model.TableName), q("id"), q(model.TableName), joinSQL, conds)
}

// existsInScopeStmt renders the existence probe behind maniflex.ScopeChecker:
// does a row with this id exist and satisfy the forced filters, counting
// soft-deleted rows as present?
//
// It omits softDeleteCond on purpose — a soft-deleted record's history must stay
// readable by whoever could read the record, and the delete entry is usually the
// one being looked for. The scope filters are applied in full, so "deleted" never
// means "unscoped": a caller still cannot see another tenant's row this way.
//
// Selecting a literal keeps the contract honest at the SQL level too: this
// statement cannot return a soft-deleted row's contents even by accident.
func existsInScopeStmt(model *maniflex.ModelMeta, id string,
	filters []*maniflex.FilterExpr, driver maniflex.DriverType,
) (string, []any) {
	p := &ph{driver: driver}
	conds := []string{fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id))}
	if len(filters) > 0 {
		if extra := filterConds(model, filters, driver, p); extra != "" {
			conds = append(conds, extra)
		}
	}
	joinSQL := buildJoins(model, filters, nil)
	return fmt.Sprintf("SELECT 1 FROM %s%s WHERE %s LIMIT 1",
		q(model.TableName), joinSQL, strings.Join(conds, " AND ")), p.args
}

// ExistsInScope implements maniflex.ScopeChecker.
func (a *Adapter) ExistsInScope(ctx context.Context, model *maniflex.ModelMeta, id string,
	filters []*maniflex.FilterExpr,
) (bool, error) {
	query, args := existsInScopeStmt(model, id, filters, a.driver)
	var one int
	err := a.readDb.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ExistsInScope query: %w", err)
	}
	return true, nil
}

// uniqueIndexFailure builds the migration error for a UNIQUE index that could
// not be created (roadmap 10.3).
//
// This is fatal where a plain index's failure is only warned about, and the
// difference is not severity but kind: a missing plain index costs a table scan,
// which the application survives, while a missing unique index means the model
// declares a constraint the database is not enforcing. Every subsequent write
// that should have been refused is accepted, silently, and nothing surfaces it
// until something downstream trips over the duplicates. `mfx:"unique"` promising
// a guarantee the schema does not have is the bug this refuses to ship with.
//
// By far the most common cause is data that already violates the constraint, so
// the message says so outright: the remedy is to de-duplicate, not to retry.
func uniqueIndexFailure(table, index string, cols []string, err error) error {
	return fmt.Errorf(
		"AutoMigrate: could not create unique index %s on %s(%s): %w — the model declares "+
			"these column(s) unique, so the server will not start without the constraint. "+
			"The usual cause is rows that already violate it; de-duplicate them and start again",
		index, table, strings.Join(cols, ", "), err)
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
				// Bind the locale key as a parameter so it can never break out of
				// the JSON-path expression (SEC-1). The ::text cast pins the ->>
				// overload to object-field access regardless of how the driver
				// types the parameter.
				return col + "->>" + p.add(f.LocaleKey) + "::text"
			}
			// SQLite: bind the whole "$.<key>" path as a parameter.
			return "json_extract(" + col + ", " + p.add("$."+f.LocaleKey) + ")"
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
	case maniflex.OpContains, maniflex.OpStartsWith, maniflex.OpEndsWith:
		return substringCond(col, op, val, driver, p)
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
	// Fail closed. An operator this switch does not implement cannot be turned
	// into a predicate, and the choice is between a condition that matches nothing
	// and one that matches everything. Matching everything deletes the filter: a
	// list returns the whole table, and a forced filter — a tenant scope — stops
	// scoping, on reads and on the writes that read back through it. Matching
	// nothing is wrong too, but it is wrong in the direction that shows up on the
	// first request rather than the first breach. maniflex's DB step rejects such
	// a filter outright (validateFilterOperators); this is the backstop for the
	// paths that do not run it, and for a filter built after it ran.
	return "1=0"
}

// substringCond builds the condition for contains / starts_with / ends_with: a
// case-insensitive LIKE against a pattern whose metacharacters are escaped, so a
// value of "50%" matches the literal "50%" instead of everything starting with 50.
//
// The ESCAPE clause is spelled out because the drivers disagree without it —
// SQLite has no escape character by default, Postgres has a backslash — and the
// same filter must mean the same thing on both.
func substringCond(col string, op maniflex.FilterOperator, val any, driver maniflex.DriverType, p *ph) string {
	pattern := maniflex.LikePattern(op, val)
	if driver == maniflex.Postgres {
		return fmt.Sprintf("%s ILIKE %s ESCAPE '\\'", col, p.add(pattern))
	}
	return fmt.Sprintf("LOWER(%s) LIKE LOWER(%s) ESCAPE '\\'", col, p.add(pattern))
}

// betweenCond expands a "lo,hi" value into "col >= lo AND col <= hi". The two
// bounds are validated to be present upstream in ParseFilterParam, so only a
// hand-built FilterExpr reaches here malformed; it degrades to a false predicate
// rather than emitting broken SQL. False, not true, for the reason buildCond
// gives: a between with one bound is a filter whose author meant to exclude
// something, and answering "everything matches" is the one reading that cannot
// be right.
func betweenCond(col string, val any, p *ph) string {
	vals := maniflex.SplitCSV(fmt.Sprint(val))
	if len(vals) != 2 {
		return "1=0"
	}
	return fmt.Sprintf("(%s >= %s AND %s <= %s)", col, p.add(vals[0]), col, p.add(vals[1]))
}

// inCond expands a "a,b,c" value into "col IN (?, ?, ?)". At least one value is
// required upstream in ParseFilterParam; an empty list here (a hand-built
// FilterExpr from the typed API) degrades to a constant predicate rather than
// emitting "col IN ()", which is a syntax error on every driver. An empty IN
// matches nothing and an empty NOT IN matches everything, which is what the set
// semantics say.
func inCond(col string, val any, p *ph, negate bool) string {
	vals := maniflex.SplitCSV(fmt.Sprint(val))
	if len(vals) == 0 {
		if negate {
			return "1=1"
		}
		return "1=0"
	}
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

func buildOrder(model *maniflex.ModelMeta, sorts []maniflex.SortExpr, driver maniflex.DriverType, p *ph) string {
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
				// Bind the locale key as a parameter so it can never break out
				// of the JSON-path expression (SEC-2), matching the filter sink.
				col = base + "->>" + p.add(s.LocaleKey) + "::text"
			} else {
				col = "json_extract(" + base + ", " + p.add("$."+s.LocaleKey) + ")"
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

func populateIncludes(ctx context.Context, runner queryRunner, reg maniflex.RegistryAccessor, driver maniflex.DriverType, model *maniflex.ModelMeta, rows []map[string]any, qp *maniflex.QueryParams) error {
	if qp == nil || len(rows) == 0 || len(qp.Includes) == 0 || reg == nil {
		return nil
	}
	scope := forcedScope(qp)
	for _, key := range qp.Includes {
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
			if err := populateBelongsTo(ctx, runner, driver, relMeta, rel, rows, scope); err != nil {
				return err
			}
		case maniflex.HasMany:
			if err := populateHasMany(ctx, runner, driver, relMeta, rel, rows, scope); err != nil {
				return err
			}
		case maniflex.ManyToMany:
			if err := populateManyToMany(ctx, runner, driver, reg, relMeta, rel, rows, scope); err != nil {
				return err
			}
		}
	}
	return nil
}

func populateBelongsTo(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any, scope []*maniflex.FilterExpr) error {
	fkSet := map[string]bool{}
	for _, row := range rows {
		if v := row[rel.FKColumn]; v != nil {
			fkSet[fmt.Sprint(v)] = true
		}
	}
	if len(fkSet) == 0 {
		return nil
	}
	relRows, err := fetchByIDs(ctx, runner, driver, relMeta, fkSet, scope)
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

func populateHasMany(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any, scope []*maniflex.FilterExpr) error {
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
	if c := includeScopeCond(relMeta, scope, driver, p); c != "" {
		conditions = append(conditions, c)
	}

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
// throughPayload builds the "_through" object exposed on each row of a
// many-to-many include: the junction's own payload columns, minus the two
// foreign keys and id, which say nothing the response does not already carry.
//
// It is built from the junction model's declared fields rather than from the
// result row's columns (audit MS-10). Copying the row wholesale ignored the
// junction's tags, so a mfx:"hidden" invited_by or a mfx:"writeonly" secret_note
// surfaced verbatim on every include — the junction is a model like any other,
// and its tags were the one thing the payload did not consult. Response-step
// filtering does not catch it either: toJSONMap preserves underscore-prefixed
// keys as framework-reserved and passes them through untouched.
//
// Excluded, in addition to the tags that hide a field from responses anywhere:
//
//   - mfx:"encrypted" columns and their _hmac companions. The junction payload
//     has no decryption pass, so the alternative is emitting ciphertext, which
//     exposes the envelope and is of no use to a client.
//   - Columns the junction model does not declare. A column present in the table
//     but absent from the model is drift; dumping it is how this leaked in the
//     first place.
//
// A junction whose model cannot be resolved yields no payload at all. That is
// the fail-closed direction: without the model there are no tags to honour, and
// an unfiltered dump is exactly what this fixes.
func throughPayload(junctionMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta,
	jrow map[string]any,
) map[string]any {
	if junctionMeta == nil {
		return nil
	}
	skip := map[string]bool{rel.ThroughLocalFK: true, rel.ThroughRemoteFK: true, "id": true}
	out := make(map[string]any, len(junctionMeta.Fields))
	for i := range junctionMeta.Fields {
		f := &junctionMeta.Fields[i]
		if skip[f.Tags.DBName] || f.Tags.Hidden || f.Tags.WriteOnly || f.Tags.Encrypted {
			continue
		}
		if v, ok := jrow[f.Tags.DBName]; ok {
			out[f.Tags.JSONName] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func populateManyToMany(ctx context.Context, runner queryRunner, driver maniflex.DriverType, reg maniflex.RegistryAccessor, relMeta *maniflex.ModelMeta, rel *maniflex.RelationMeta, rows []map[string]any, scope []*maniflex.FilterExpr) error {
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

	var junctionMeta *maniflex.ModelMeta
	if reg != nil && rel.ThroughModel != "" {
		if jm, ok := reg.Get(rel.ThroughModel); ok {
			junctionMeta = jm
		}
	}
	for _, jrow := range junctions {
		pid := fmt.Sprint(jrow[rel.ThroughLocalFK])
		rid := fmt.Sprint(jrow[rel.ThroughRemoteFK])
		if pid == "" || rid == "" {
			continue
		}
		remoteIDSet[rid] = true
		parentToRemotes[pid] = append(parentToRemotes[pid], pair{
			remoteID: rid,
			through:  throughPayload(junctionMeta, rel, jrow),
		})
	}

	// Step 3: batch-fetch related model rows
	relRows, err := fetchByIDs(ctx, runner, driver, relMeta, remoteIDSet, scope)
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

// forcedScope returns the server-imposed filters on a request — the tenancy and
// force-filter scope — which the include loaders re-apply to the related model
// (audit MS-9).
func forcedScope(qp *maniflex.QueryParams) []*maniflex.FilterExpr {
	if qp == nil {
		return nil
	}
	var out []*maniflex.FilterExpr
	for _, f := range qp.Filters {
		if f != nil && f.Forced {
			out = append(out, f)
		}
	}
	return out
}

// includeScopeCond renders the part of a request's scope that also applies to an
// included model: every forced filter whose column that model actually has.
//
// Includes used to be fetched with the soft-delete condition and nothing else,
// so a tenancy filter constrained the primary read and stopped at the relation
// boundary. That is not merely an omission — it is exploitable in both
// directions: a caller who can set a foreign key (or write a junction row) puts
// their record inside another tenant's ?include=, and a many-to-many attach
// pulls another tenant's record into their own response. Verified for MS-9: a
// child row belonging to tenant-b appeared inside tenant-a's include.
//
// Only filters naming a column the related model carries are applied. A related
// model with no such column is left unscoped, which is the right answer for the
// case that produces it — a shared lookup table (currencies, categories) is not
// tenant-partitioned and has no column to scope by. The cost of that choice is
// that a model which *is* partitioned by something this scope cannot express
// stays unscoped; scoping such a relation is the application's job, as it always
// was for the junction write itself.
//
// Nested (relation-path) filters are skipped: they are written against the
// primary model's relations and mean nothing on the related table.
func includeScopeCond(relMeta *maniflex.ModelMeta, scope []*maniflex.FilterExpr,
	driver maniflex.DriverType, p *ph,
) string {
	if len(scope) == 0 {
		return ""
	}
	applicable := make([]*maniflex.FilterExpr, 0, len(scope))
	for _, f := range scope {
		if f.IsNested || f.IsLocale {
			continue
		}
		if relMeta.FieldByDBName(f.Field) == nil {
			continue
		}
		applicable = append(applicable, f)
	}
	if len(applicable) == 0 {
		return ""
	}
	return filterConds(relMeta, applicable, driver, p)
}

func fetchByIDs(ctx context.Context, runner queryRunner, driver maniflex.DriverType, relMeta *maniflex.ModelMeta, idSet map[string]bool, scope []*maniflex.FilterExpr) ([]map[string]any, error) {
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
	if c := includeScopeCond(relMeta, scope, driver, p); c != "" {
		conditions = append(conditions, c)
	}

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
	query = rebind(a.driver, query)
	switch classifyRaw(query) {
	case rawSelect:
		rows, err := a.readDb.QueryContext(ctx, query, args...)
		return RawSqlResult{rowsVal: rows, rowsErr: err}
	case rawReturning:
		// A data-modifying statement with RETURNING yields a result set and must
		// run on the write pool. Routing it through QueryContext (not ExecContext)
		// is what preserves the returned rows.
		rows, err := a.writeDb.QueryContext(ctx, query, args...)
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
