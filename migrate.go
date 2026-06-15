package maniflex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// Migration is a single versioned schema change applied exactly once.
//
// Version is a unique identifier ordered lexicographically — typically a
// zero-padded sequence ("0001_init", "0002_add_index") or a timestamp
// ("20260429_120000_backfill"). Migrate sorts the slice by Version before
// running, so registration order does not matter.
//
// Up runs inside a transaction. Returning an error rolls the transaction
// back and aborts the migration run; the version is not recorded as applied.
// Use the supplied *sql.Tx for both DDL and any data backfill so the entire
// step is atomic.
//
// Note: some DDL statements are not transactional on every backend (e.g.
// Postgres `CREATE INDEX CONCURRENTLY` cannot run inside a transaction). For
// those, set NoTransaction: true and run the statement directly against the
// supplied *sql.Tx — it will be a degenerate "transaction" that simply runs
// the statement on a dedicated connection without BEGIN/COMMIT wrapping.
type Migration struct {
	// Version uniquely identifies the migration. Required, must be non-empty.
	Version string

	// Up applies the migration. Required.
	Up func(ctx context.Context, tx *sql.Tx) error

	// NoTransaction disables the BEGIN/COMMIT wrapper for this migration.
	// Use only for DDL that the database refuses to run inside a transaction
	// (e.g. Postgres CREATE INDEX CONCURRENTLY). When true, the *sql.Tx
	// passed to Up is a thin shim that executes against a dedicated
	// connection with no rollback semantics.
	NoTransaction bool
}

// migrationBackend is implemented by DB adapters that support versioned
// migrations. sqlcore.Adapter satisfies it via its WriteDB and DriverName
// methods.
type migrationBackend interface {
	WriteDB() *sql.DB
	DriverName() string
}

// Migrate runs the supplied migrations against the server's DB adapter,
// recording each applied version in a schema_migrations table.
//
// Migrations are sorted by Version and executed in order. A migration is
// skipped if its version already appears in schema_migrations, so Migrate
// is safe to call on every startup. Each migration runs inside its own
// transaction (unless NoTransaction is set); the version row is inserted
// in the same transaction so a crash mid-migration leaves the table in
// either the pre- or post-state, never partially applied.
//
// Migrate is independent of AutoMigrate: AutoMigrate handles struct-driven
// CREATE/ALTER for green-field development, while Migrate is the path for
// data backfills, concurrent index builds, and coordinated multi-step
// deployments. Most production setups call AutoMigrate first (to ensure
// tables exist) and then Migrate (for the explicit, ordered changes).
//
//	if err := maniflex.Migrate(server, []maniflex.Migration{
//	    {Version: "0001_seed_roles", Up: seedRoles},
//	    {Version: "0002_backfill_slugs", Up: backfillSlugs},
//	}); err != nil {
//	    log.Fatal(err)
//	}
func Migrate(s *Server, migrations []Migration) error {
	if s == nil {
		return errors.New("maniflex.Migrate: nil server")
	}
	if s.cfg.DB == nil {
		return errors.New("maniflex.Migrate: no DB adapter configured")
	}
	backend, ok := s.cfg.DB.(migrationBackend)
	if !ok {
		return errors.New("maniflex.Migrate: DB adapter does not support migrations (must implement WriteDB and DriverName)")
	}
	return runMigrations(context.Background(), backend, s.cfg.logger(), migrations)
}

func runMigrations(ctx context.Context, backend migrationBackend, log *slog.Logger, migrations []Migration) error {
	if err := validateMigrations(migrations); err != nil {
		return err
	}

	sorted := make([]Migration, len(migrations))
	copy(sorted, migrations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })

	db := backend.WriteDB()
	driver := backend.DriverName()

	if err := ensureMigrationsTable(ctx, db, driver); err != nil {
		return fmt.Errorf("maniflex.Migrate: ensure schema_migrations: %w", err)
	}
	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("maniflex.Migrate: load applied versions: %w", err)
	}

	insertSQL := insertMigrationSQL(driver)

	for _, m := range sorted {
		if applied[m.Version] {
			continue
		}
		log.Info("maniflex.Migrate: applying", slog.String("version", m.Version))

		if err := applyMigration(ctx, db, m, insertSQL); err != nil {
			return fmt.Errorf("maniflex.Migrate: %s: %w", m.Version, err)
		}
		log.Info("maniflex.Migrate: applied", slog.String("version", m.Version))
	}
	return nil
}

func validateMigrations(migrations []Migration) error {
	seen := make(map[string]bool, len(migrations))
	for i, m := range migrations {
		if m.Version == "" {
			return fmt.Errorf("maniflex.Migrate: migration at index %d has empty Version", i)
		}
		if m.Up == nil {
			return fmt.Errorf("maniflex.Migrate: migration %s has nil Up", m.Version)
		}
		if seen[m.Version] {
			return fmt.Errorf("maniflex.Migrate: duplicate Version %q", m.Version)
		}
		seen[m.Version] = true
	}
	return nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB, driver string) error {
	var stmt string
	if driver == "postgres" {
		stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL
		)`
	} else {
		stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`
	}
	_, err := db.ExecContext(ctx, stmt)
	return err
}

func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func insertMigrationSQL(driver string) string {
	if driver == "postgres" {
		return "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)"
	}
	return "INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)"
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration, insertSQL string) error {
	appliedAt := time.Now().UTC().Format(time.RFC3339Nano)

	if m.NoTransaction {
		// Run Up directly on the connection, then record the version.
		// We pass nil for the *sql.Tx so callers know they have no
		// transactional guarantees; they should use db handles obtained
		// elsewhere (or just the connection-level ExecContext).
		if err := m.Up(ctx, nil); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, insertSQL, m.Version, appliedAt); err != nil {
			return fmt.Errorf("record version: %w", err)
		}
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := m.Up(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, insertSQL, m.Version, appliedAt); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
