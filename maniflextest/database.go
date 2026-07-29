package maniflextest

import (
	"context"
	"fmt"
	"strings"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/postgres"
	"github.com/xaleel/maniflex/db/sqlite"
)

// Database describes a test database and its optional teardown work.
// The harness closes Adapter after Cleanup returns.
type Database struct {
	Adapter maniflex.DBAdapter
	Cleanup func(context.Context) error
}

// DatabaseFactory opens an isolated database for one test server.
type DatabaseFactory func(context.Context, maniflex.RegistryAccessor) (Database, error)

// SQLite returns a database factory backed by SQLite. With no path, each test
// gets a distinct shared in-memory database. Pass one path to exercise a
// file-backed database.
func SQLite(path ...string) DatabaseFactory {
	return func(_ context.Context, reg maniflex.RegistryAccessor) (Database, error) {
		if len(path) > 1 {
			return Database{}, fmt.Errorf("maniflextest: SQLite accepts at most one path")
		}
		name := ":memory:"
		if len(path) == 1 {
			name = path[0]
		}
		db, err := sqlite.Open(name, reg)
		if err != nil {
			return Database{}, err
		}
		return Database{Adapter: db}, nil
	}
}

// Postgres returns a factory that creates a random schema in dsn for each test
// and drops it during cleanup. The database and credentials in dsn must already
// exist and permit CREATE SCHEMA and DROP SCHEMA.
func Postgres(dsn string) DatabaseFactory {
	return func(_ context.Context, reg maniflex.RegistryAccessor) (Database, error) {
		if strings.TrimSpace(dsn) == "" {
			return Database{}, fmt.Errorf("maniflextest: Postgres requires a DSN")
		}

		schema := "maniflextest_" + strings.ToLower(maniflex.RandomString(12, maniflex.ALPHANUM))
		db, err := postgres.OpenWithConfig(
			dsn,
			"",
			reg,
			postgres.PoolConfig{},
			postgres.PoolConfig{},
			postgres.SessionConfig{
				ApplicationName: "maniflextest",
				SchemaName:      &schema,
			},
		)
		if err != nil {
			return Database{}, err
		}

		return Database{
			Adapter: db,
			Cleanup: func(ctx context.Context) error {
				_, err := db.Raw(ctx, "DROP SCHEMA "+schema+" CASCADE").RowsAffected()
				return err
			},
		}, nil
	}
}
