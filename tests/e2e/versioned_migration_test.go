package e2e

// versioned_migration_test.go covers maniflex.Migrate — the versioned migration
// helper introduced alongside (and complementary to) AutoMigrate. Where
// AutoMigrate handles struct-driven structural diffing, Migrate runs ordered
// SQL/Go migrations exactly once, recorded in a schema_migrations table.
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestMigrate

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// openServer returns a Server server backed by a fresh on-disk SQLite database.
func openServer(t *testing.T) (*maniflex.Server, string) {
	t.Helper()
	path, _ := tempDB(t)
	s := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	db, err := sqlite.Open(path, s.Registry())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	s.SetDB(db)
	t.Cleanup(func() { db.Close() })
	return s, path
}

// rawDB opens an independent connection so tests can inspect tables outside
// the Server adapter's connection pool.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("rawDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate(t *testing.T) {
	t.Parallel()

	t.Run("creates_schema_migrations_table_and_records_versions", func(t *testing.T) {
		t.Parallel()
		s, path := openServer(t)

		ran := []string{}
		mk := func(v string) maniflex.Migration {
			return maniflex.Migration{
				Version: v,
				Up: func(ctx context.Context, tx *sql.Tx) error {
					ran = append(ran, v)
					return nil
				},
			}
		}
		err := maniflex.Migrate(s, []maniflex.Migration{mk("0002_b"), mk("0001_a")})
		if err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		// Order: sorted by Version.
		if len(ran) != 2 || ran[0] != "0001_a" || ran[1] != "0002_b" {
			t.Errorf("expected sorted execution, got %v", ran)
		}

		// schema_migrations contains both versions.
		db := rawDB(t, path)
		rows, err := db.Query("SELECT version FROM schema_migrations ORDER BY version")
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		var versions []string
		for rows.Next() {
			var v string
			rows.Scan(&v)
			versions = append(versions, v)
		}
		if len(versions) != 2 || versions[0] != "0001_a" || versions[1] != "0002_b" {
			t.Errorf("expected versions [0001_a 0002_b], got %v", versions)
		}
	})

	t.Run("already_applied_migrations_are_skipped", func(t *testing.T) {
		t.Parallel()
		s, _ := openServer(t)

		count := 0
		m := maniflex.Migration{
			Version: "0001_only",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				count++
				return nil
			},
		}

		if err := maniflex.Migrate(s, []maniflex.Migration{m}); err != nil {
			t.Fatalf("first run: %v", err)
		}
		if err := maniflex.Migrate(s, []maniflex.Migration{m}); err != nil {
			t.Fatalf("second run: %v", err)
		}
		if count != 1 {
			t.Errorf("expected Up to run exactly once, ran %d times", count)
		}
	})

	t.Run("error_in_up_rolls_back_and_does_not_record_version", func(t *testing.T) {
		t.Parallel()
		s, path := openServer(t)

		boom := errors.New("kaboom")
		m := maniflex.Migration{
			Version: "0001_breaks",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				// Make a real DDL change inside the tx, then fail. The DDL
				// must be rolled back when we return the error.
				if _, err := tx.ExecContext(ctx, "CREATE TABLE never_committed (id TEXT)"); err != nil {
					return err
				}
				return boom
			},
		}
		err := maniflex.Migrate(s, []maniflex.Migration{m})
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("expected boom error, got %v", err)
		}

		db := rawDB(t, path)
		// Version not recorded.
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("expected 0 recorded versions on rollback, got %d", n)
		}
		// DDL rolled back.
		var name string
		err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='never_committed'").Scan(&name)
		if err != sql.ErrNoRows {
			t.Errorf("expected never_committed table to be absent (rolled back), got name=%q err=%v", name, err)
		}
	})

	t.Run("up_runs_inside_transaction_and_can_create_tables", func(t *testing.T) {
		t.Parallel()
		s, path := openServer(t)

		m := maniflex.Migration{
			Version: "0001_create_widgets",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`CREATE TABLE widgets (id TEXT PRIMARY KEY, name TEXT NOT NULL)`)
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx,
					`INSERT INTO widgets (id, name) VALUES ('w1', 'first')`)
				return err
			},
		}
		if err := maniflex.Migrate(s, []maniflex.Migration{m}); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		db := rawDB(t, path)
		var name string
		if err := db.QueryRow("SELECT name FROM widgets WHERE id='w1'").Scan(&name); err != nil {
			t.Fatalf("widget read: %v", err)
		}
		if name != "first" {
			t.Errorf("expected widget name 'first', got %q", name)
		}
	})

	t.Run("subsequent_run_applies_only_new_migrations", func(t *testing.T) {
		t.Parallel()
		s, _ := openServer(t)

		ran := []string{}
		m1 := maniflex.Migration{
			Version: "0001",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				ran = append(ran, "0001")
				return nil
			},
		}
		if err := maniflex.Migrate(s, []maniflex.Migration{m1}); err != nil {
			t.Fatalf("first migrate: %v", err)
		}

		m2 := maniflex.Migration{
			Version: "0002",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				ran = append(ran, "0002")
				return nil
			},
		}
		if err := maniflex.Migrate(s, []maniflex.Migration{m1, m2}); err != nil {
			t.Fatalf("second migrate: %v", err)
		}

		// 0001 ran once on first call; only 0002 ran on second call.
		if len(ran) != 2 || ran[0] != "0001" || ran[1] != "0002" {
			t.Errorf("expected ran=[0001 0002], got %v", ran)
		}
	})

	t.Run("validation_rejects_empty_version", func(t *testing.T) {
		t.Parallel()
		s, _ := openServer(t)
		err := maniflex.Migrate(s, []maniflex.Migration{
			{Version: "", Up: func(ctx context.Context, tx *sql.Tx) error { return nil }},
		})
		if err == nil {
			t.Fatal("expected error for empty version")
		}
	})

	t.Run("validation_rejects_nil_up", func(t *testing.T) {
		t.Parallel()
		s, _ := openServer(t)
		err := maniflex.Migrate(s, []maniflex.Migration{{Version: "0001", Up: nil}})
		if err == nil {
			t.Fatal("expected error for nil Up")
		}
	})

	t.Run("validation_rejects_duplicate_version", func(t *testing.T) {
		t.Parallel()
		s, _ := openServer(t)
		noop := func(ctx context.Context, tx *sql.Tx) error { return nil }
		err := maniflex.Migrate(s, []maniflex.Migration{
			{Version: "0001", Up: noop},
			{Version: "0001", Up: noop},
		})
		if err == nil {
			t.Fatal("expected error for duplicate version")
		}
	})

	t.Run("nil_db_adapter_returns_error", func(t *testing.T) {
		t.Parallel()
		s := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
		err := maniflex.Migrate(s, nil)
		if err == nil {
			t.Fatal("expected error when DB adapter is nil")
		}
	})

	t.Run("works_alongside_automigrate_for_data_backfill", func(t *testing.T) {
		t.Parallel()
		// Realistic flow: AutoMigrate creates the table, Migrate runs a
		// data backfill against it.
		path, cleanup := tempDB(t)
		defer cleanup()

		s := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
		s.MustRegister(productV1{}, maniflex.ModelConfig{TableName: "products"})
		db, err := sqlite.Open(path, s.Registry())
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		s.SetDB(db)
		t.Cleanup(func() { db.Close() })

		if err := db.AutoMigrate(context.Background(), s.Registry()); err != nil {
			t.Fatalf("AutoMigrate: %v", err)
		}

		backfill := maniflex.Migration{
			Version: "0001_seed_default_product",
			Up: func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO products (id, name, created_at, updated_at)
					 VALUES ('seed-1', 'Default', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
				return err
			},
		}
		if err := maniflex.Migrate(s, []maniflex.Migration{backfill}); err != nil {
			t.Fatalf("Migrate: %v", err)
		}

		raw := rawDB(t, path)
		var name string
		if err := raw.QueryRow("SELECT name FROM products WHERE id='seed-1'").Scan(&name); err != nil {
			t.Fatalf("seed read: %v", err)
		}
		if name != "Default" {
			t.Errorf("expected seeded name 'Default', got %q", name)
		}
	})
}
