package e2e

// migration_test.go tests the AutoMigrate schema-diffing behaviour introduced
// in db/sqlcore/adapter.go.
//
// The core scenarios:
//   - New columns are added via ALTER TABLE ADD COLUMN (add a field to a struct)
//   - Existing rows survive a column addition and carry the zero/default value
//   - Removed columns produce a WARN log, not an error or dropped data
//   - Re-running AutoMigrate on an unchanged schema is a safe no-op
//   - New tables are still created on first run
//   - Nullable (pointer) columns are added correctly
//   - Explicit mfx:"default:X" is used on the new column
//   - Concurrent AutoMigrate calls do not produce errors (idempotent)
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestAutoMigrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"maniflex"
	"maniflex/db/sqlcore"
	"maniflex/db/sqlite"
	"maniflex/tests/e2e/testutil"
)

// ── Model versions for schema-evolution tests ─────────────────────────────────
//
// We simulate adding fields to a model by registering two different struct
// definitions against the same table name. "V1" is the initial schema;
// "V2" adds new columns. Both use the same TableName so they target the same
// database table.

// productV1 is the initial schema: just a name.
type productV1 struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required,filterable,sortable"`
}

// productV2 adds three new columns of different types.
type productV2 struct {
	maniflex.BaseModel
	Name     string  `json:"name"     db:"name"     mfx:"required,filterable,sortable"`
	Price    float64 `json:"price"    db:"price"    mfx:"filterable,sortable"`
	InStock  bool    `json:"in_stock" db:"in_stock" mfx:"filterable,sortable"`
	Category string  `json:"category" db:"category" mfx:"filterable,sortable"`
}

// productV3 adds a nullable column (pointer type).
type productV3 struct {
	maniflex.BaseModel
	Name        string  `json:"name"         db:"name"         mfx:"required,filterable,sortable"`
	Price       float64 `json:"price"        db:"price"        mfx:"filterable,sortable"`
	InStock     bool    `json:"in_stock"     db:"in_stock"     mfx:"filterable,sortable"`
	Category    string  `json:"category"     db:"category"     mfx:"filterable,sortable"`
	Description *string `json:"description"  db:"description"  mfx:"filterable"`
}

// productV4 adds a column with an explicit mfx:"default:X" tag.
type productV4 struct {
	maniflex.BaseModel
	Name   string `json:"name"     db:"name"     mfx:"required,filterable,sortable"`
	Status string `json:"status"   db:"status"   mfx:"filterable,sortable,default:active"`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// tempDB opens a real file-backed SQLite DB for migration tests. File-backed is
// required because we need to open two separate Maniflex instances against the same
// database (once to create the schema, once to migrate it).
func tempDB(t *testing.T) (path string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "test.db")
	return path, func() { os.Remove(path) }
}

// openAdapter opens a raw *sql.DB and wraps it in a sqlcore.Adapter, sharing
// the same connection for both reads and writes (simpler for migration tests).
func openAdapter(t *testing.T, path string, reg maniflex.RegistryAccessor) *sqlcore.Adapter {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	// busy_timeout tells SQLite to wait up to 10s for the write lock instead of
	// immediately returning SQLITE_BUSY. This is essential for the concurrent
	// migration test where multiple goroutines compete for the same write lock.
	// journal_mode=WAL allows readers to proceed while the writer holds the lock.
	db.SetMaxOpenConns(1) // serialise writers — mirrors sqlite.Open behaviour
	db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000;")
	t.Cleanup(func() { db.Close() })
	adapter := sqlcore.New(db, db, maniflex.SQLite, reg)
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	return adapter
}

// columnNames queries PRAGMA table_info to return the set of column names
// currently in the named table.
func columnNames(t *testing.T, path, table string) map[string]bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("columnNames open: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(),
		fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma row: %v", err)
		}
		cols[name] = true
	}
	return cols
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestAutoMigrate(t *testing.T) {
	t.Parallel()

	// ── Core: new columns are added ───────────────────────────────────────────

	t.Run("new_string_column_added_to_existing_table", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		// First boot: create table with V1 schema
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		s1.SetDB(a1)
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v1 migrate: %v", err)
		}

		// Verify V1 columns exist
		cols := columnNames(t, path, "products")
		testutil.AssertEqual(t, "v1 has name", cols["name"], true)
		if cols["price"] || cols["in_stock"] || cols["category"] {
			t.Error("v1 schema must not yet have price/in_stock/category")
		}

		// Second boot: migrate to V2 (adds price, in_stock, category)
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		s2.SetDB(a2)
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		cols2 := columnNames(t, path, "products")
		testutil.AssertEqual(t, "v2 has name", cols2["name"], true)
		testutil.AssertEqual(t, "v2 has price", cols2["price"], true)
		testutil.AssertEqual(t, "v2 has in_stock", cols2["in_stock"], true)
		testutil.AssertEqual(t, "v2 has category", cols2["category"], true)
	})

	t.Run("nullable_pointer_column_added", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		// Boot V2
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		// Boot V3 (adds nullable *string description)
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV3{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v3 migrate: %v", err)
		}

		cols := columnNames(t, path, "products")
		testutil.AssertEqual(t, "v3 has description", cols["description"], true)
	})

	// ── Existing data survives column addition ─────────────────────────────────

	t.Run("existing_rows_survive_column_addition", func(t *testing.T) {
		t.Parallel()
		skipSQLiteFileDBOnPostgres(t)
		path, cleanup := tempDB(t)
		defer cleanup()

		// Create table and insert a row at V1 schema
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		s1.SetDB(a1)
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v1 migrate: %v", err)
		}

		// Insert a V1 row via the HTTP server
		ts1 := testutil.NewServer(t, testutil.Options{
			Models:      []any{productV1{}, maniflex.ModelConfig{TableName: "products"}},
			DBPath:      path,
			AutoMigrate: testutil.Ptr(false), // already migrated
		})
		resp := ts1.POST("/products", map[string]any{"name": "Widget"})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()

		// Migrate to V2
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		// Open a V2 server and read the existing row — must succeed with zero values
		ts2 := testutil.NewServer(t, testutil.Options{
			Models:      []any{productV2{}, maniflex.ModelConfig{TableName: "products"}},
			DBPath:      path,
			AutoMigrate: testutil.Ptr(false),
		})
		data := ts2.GET("/products/" + id).Data()
		testutil.AssertEqual(t, "name preserved", testutil.Field(t, data, "name"), "Widget")
		testutil.AssertEqual(t, "price default 0", testutil.FloatField(t, data, "price"), float64(0))
		testutil.AssertEqual(t, "in_stock default false", testutil.BoolField(t, data, "in_stock"), false)
		testutil.AssertEqual(t, "category default empty", testutil.Field(t, data, "category"), "")
	})

	t.Run("existing_rows_get_explicit_default_value", func(t *testing.T) {
		t.Parallel()
		skipSQLiteFileDBOnPostgres(t)
		path, cleanup := tempDB(t)
		defer cleanup()

		// Create V1 (name only) and insert a row
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		s1.SetDB(a1)
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v1 migrate: %v", err)
		}
		ts1 := testutil.NewServer(t, testutil.Options{
			Models:      []any{productV1{}, maniflex.ModelConfig{TableName: "products"}},
			DBPath:      path,
			AutoMigrate: testutil.Ptr(false),
		})
		id := ts1.MustID(ts1.POST("/products", map[string]any{"name": "Widget"}))

		// Migrate to V4 (adds status with default:"active")
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV4{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v4 migrate: %v", err)
		}

		// Existing row must carry the explicit default
		ts2 := testutil.NewServer(t, testutil.Options{
			Models:      []any{productV4{}, maniflex.ModelConfig{TableName: "products"}},
			DBPath:      path,
			AutoMigrate: testutil.Ptr(false),
		})
		data := ts2.GET("/products/" + id).Data()
		testutil.AssertEqual(t, "status has explicit default",
			testutil.Field(t, data, "status"), "active")
	})

	// ── Re-run is idempotent ───────────────────────────────────────────────────

	t.Run("running_automigrate_twice_is_a_noop", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		s := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a := openAdapter(t, path, s.Registry())

		// First run
		if err := a.AutoMigrate(context.Background(), s.Registry()); err != nil {
			t.Fatalf("first migrate: %v", err)
		}
		// Second run — must not error, must not change column count
		if err := a.AutoMigrate(context.Background(), s.Registry()); err != nil {
			t.Fatalf("second migrate: %v", err)
		}

		cols := columnNames(t, path, "products")
		for _, expected := range []string{"id", "created_at", "updated_at", "name", "price", "in_stock", "category"} {
			testutil.AssertEqual(t, "column "+expected, cols[expected], true)
		}
	})

	t.Run("first_run_creates_table", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		s := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a := openAdapter(t, path, s.Registry())

		if err := a.AutoMigrate(context.Background(), s.Registry()); err != nil {
			t.Fatalf("first migrate: %v", err)
		}

		cols := columnNames(t, path, "products")
		for _, expected := range []string{"id", "created_at", "updated_at", "name"} {
			testutil.AssertEqual(t, "column "+expected+" created", cols[expected], true)
		}
	})

	// ── Removed columns: warning, not error or drop ────────────────────────────

	t.Run("removed_field_logs_warning_not_error", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		// Create with V2 (has price, in_stock, category)
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		// Now "downgrade" to V1 (struct no longer has price/in_stock/category).
		// AutoMigrate must succeed (no error) — it only warns.
		var mu sync.Mutex
		var warnMsgs []string
		handler := &captureSlogHandler{
			mu:      &mu,
			records: &[]slog.Record{},
		}
		// Override default logger for this test
		oldDefault := slog.Default()
		slog.SetDefault(slog.New(handler))
		t.Cleanup(func() { slog.SetDefault(oldDefault) })

		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v1 re-migrate must not error on removed columns: %v", err)
		}

		// Collect warning messages
		mu.Lock()
		for _, rec := range *handler.records {
			if rec.Level == slog.LevelWarn {
				var msg strings.Builder
				msg.WriteString(rec.Message)
				rec.Attrs(func(a slog.Attr) bool {
					msg.WriteString(" " + a.Key + "=" + a.Value.String())
					return true
				})
				warnMsgs = append(warnMsgs, msg.String())
			}
		}
		mu.Unlock()

		// The DB columns price, in_stock, category are no longer in V1 model →
		// must each produce a warning.
		for _, orphan := range []string{"price", "in_stock", "category"} {
			found := false
			for _, msg := range warnMsgs {
				if strings.Contains(msg, orphan) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected a WARN log mentioning removed column %q, got warnings: %v",
					orphan, warnMsgs)
			}
		}
	})

	t.Run("removed_column_data_still_in_db", func(t *testing.T) {
		t.Parallel()
		skipSQLiteFileDBOnPostgres(t)
		path, cleanup := tempDB(t)
		defer cleanup()

		// Boot V2, insert a row
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		a1 := openAdapter(t, path, s1.Registry())
		s1.SetDB(a1)
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}
		ts1 := testutil.NewServer(t, testutil.Options{
			Models:      []any{productV2{}, maniflex.ModelConfig{TableName: "products"}},
			DBPath:      path,
			AutoMigrate: testutil.Ptr(false),
		})
		id := ts1.MustID(ts1.POST("/products", map[string]any{
			"name": "Widget", "price": 9.99, "in_stock": true, "category": "tools",
		}))

		// Re-migrate with V1 (no price/in_stock/category in struct) — must not error.
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v1 re-migrate: %v", err)
		}

		// The data in the "removed" columns must still be in the DB.
		// Verify by reading the raw SQL (not through the API, which doesn't see them).
		db, _ := sql.Open("sqlite", path)
		defer db.Close()
		var price float64
		var category string
		err := db.QueryRowContext(context.Background(),
			"SELECT price, category FROM products WHERE id = ?", id,
		).Scan(&price, &category)
		if err != nil {
			t.Fatalf("direct query after v1 re-migrate: %v", err)
		}
		testutil.AssertEqual(t, "price data preserved", price, 9.99)
		testutil.AssertEqual(t, "category data preserved", category, "tools")
	})

	// ── Concurrent AutoMigrate calls ───────────────────────────────────────────

	t.Run("concurrent_automigrate_calls_are_idempotent", func(t *testing.T) {
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		// Run n concurrent AutoMigrate calls against the same file DB.
		// All must succeed — none should fail with "duplicate column name".
		const n = 5
		errs := make([]error, n)
		var wg sync.WaitGroup
		si := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		si.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		ai := openAdapter(t, path, si.Registry())
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = ai.AutoMigrate(context.Background(), si.Registry())
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("goroutine %d: AutoMigrate error: %v", i, err)
			}
		}
	})

	// ── Standard testutil.NewServer still works ────────────────────────────────

	t.Run("automigrate_works_end_to_end_via_http", func(t *testing.T) {
		t.Parallel()
		// The standard in-memory server must still function correctly after our changes.
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "am@x.com", "viewer"))
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
	})

	t.Run("multi_model_migration_adds_columns_to_all_tables", func(t *testing.T) {
		t.Parallel()

		// itemV1 is a simple item model.
		type itemV1 struct {
			maniflex.BaseModel
			Title string `json:"title" db:"title" mfx:"required"`
		}
		// itemV2 adds a notes column.
		type itemV2 struct {
			maniflex.BaseModel
			Title string `json:"title" db:"title" mfx:"required"`
			Notes string `json:"notes" db:"notes" mfx:"filterable"`
		}

		path, cleanup := tempDB(t)
		defer cleanup()

		// Boot V1 for two models
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(
			productV1{}, maniflex.ModelConfig{TableName: "products"},
			itemV1{}, maniflex.ModelConfig{TableName: "items"},
		)
		a1 := openAdapter(t, path, s1.Registry())
		if err := a1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v1 migrate: %v", err)
		}

		// Boot V2 for both models
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(
			productV2{}, maniflex.ModelConfig{TableName: "products"},
			itemV2{}, maniflex.ModelConfig{TableName: "items"},
		)
		a2 := openAdapter(t, path, s2.Registry())
		if err := a2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		// Both tables should have their new columns
		prodCols := columnNames(t, path, "products")
		testutil.AssertEqual(t, "products.price added", prodCols["price"], true)

		itemCols := columnNames(t, path, "items")
		testutil.AssertEqual(t, "items.notes added", itemCols["notes"], true)
	})

	t.Run("sqlite_open_via_package_adds_columns", func(t *testing.T) {
		// Verify the full sqlite.Open path (which handles dual read/write connections
		// and WAL setup) also correctly adds new columns.
		t.Parallel()
		path, cleanup := tempDB(t)
		defer cleanup()

		// First: create with V1 via sqlite.Open
		s1 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s1.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		db1, err := sqlite.Open(path, s1.Registry())
		if err != nil {
			t.Fatalf("sqlite open v1: %v", err)
		}
		s1.SetDB(db1)
		if err := db1.AutoMigrate(context.Background(), s1.Registry()); err != nil {
			t.Fatalf("v1 migrate: %v", err)
		}
		db1.Close()

		// Second: migrate to V2 via sqlite.Open
		s2 := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: false})
		s2.MustRegister(productV2{}, maniflex.ModelConfig{TableName: "products"})
		db2, err := sqlite.Open(path, s2.Registry())
		if err != nil {
			t.Fatalf("sqlite open v2: %v", err)
		}
		t.Cleanup(func() { db2.Close() })
		if err := db2.AutoMigrate(context.Background(), s2.Registry()); err != nil {
			t.Fatalf("v2 migrate: %v", err)
		}

		cols := columnNames(t, path, "products")
		testutil.AssertEqual(t, "price added via sqlite.Open", cols["price"], true)
		testutil.AssertEqual(t, "in_stock added via sqlite.Open", cols["in_stock"], true)
		testutil.AssertEqual(t, "category added via sqlite.Open", cols["category"], true)
	})
}
