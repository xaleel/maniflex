package e2e

// Two replicas booting at the same moment both migrate. The column paths absorb
// that race — Postgres gets ALTER TABLE ADD COLUMN IF NOT EXISTS, SQLite gets
// duplicate-column tolerance — but the Postgres foreign-key pass probed
// information_schema and then issued ALTER TABLE ADD CONSTRAINT, with no
// IF NOT EXISTS available and no tolerance for losing the race. The loser's boot
// failed against a schema that was already correct.
//
// This is Postgres-only by construction: SQLite declares its foreign keys inline
// in CREATE TABLE, so migratePostgresForeignKeys is a no-op there — which is why
// the existing SQLite concurrency test never covered this.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/postgres"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type FKRaceAuthor struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

type FKRaceBook struct {
	maniflex.BaseModel
	Title        string        `json:"title"`
	AuthorID     string        `json:"author_id" mfx:"relation:FKRaceAuthor;onDelete:cascade"`
	FKRaceAuthor *FKRaceAuthor `json:"author,omitempty"`
}

func fkRaceAuthorConfig() maniflex.ModelConfig {
	return maniflex.ModelConfig{TableName: "fk_race_authors"}
}
func fkRaceBookConfig() maniflex.ModelConfig {
	return maniflex.ModelConfig{TableName: "fk_race_books"}
}

func TestAutoMigrate_ConcurrentReplicasAgreeOnForeignKeys(t *testing.T) {
	if !testutil.IsPostgres() {
		t.Skip("foreign keys are added by a separate pass on Postgres only; " +
			"SQLite declares them inline in CREATE TABLE")
	}
	dsn := testutil.PostgresDSN()
	if dsn == "" {
		t.Skip("no Postgres DSN configured")
	}
	ctx := context.Background()

	// A schema shared by every replica — the point is that they collide.
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("schema name: %v", err)
	}
	schema := "mfxfk_" + hex.EncodeToString(raw[:])
	public := "public"
	admin, err := postgres.OpenWithConfig(dsn, "", &maniflex.Registry{},
		postgres.PoolConfig{MaxOpenConns: 2}, postgres.PoolConfig{MaxOpenConns: 2},
		postgres.SessionConfig{SchemaName: &public})
	if err != nil {
		t.Fatalf("open admin adapter: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	if _, err := admin.Raw(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)).RowsAffected(); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Raw(context.Background(),
			fmt.Sprintf("DROP SCHEMA %s CASCADE", schema)).RowsAffected()
	})

	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(FKRaceAuthor{}, fkRaceAuthorConfig())
	srv.MustRegister(FKRaceBook{}, fkRaceBookConfig())
	reg := srv.Registry()

	const replicas = 4
	adapters := make([]maniflex.DBAdapter, replicas)
	for i := range adapters {
		a, err := postgres.OpenWithConfig(dsn, "", reg,
			postgres.PoolConfig{MaxOpenConns: 2}, postgres.PoolConfig{MaxOpenConns: 2},
			postgres.SessionConfig{SchemaName: &schema})
		if err != nil {
			t.Fatalf("open replica %d: %v", i, err)
		}
		adapters[i] = a
		t.Cleanup(func() { a.Close() })
	}

	// Release them together so the probe-then-add window actually overlaps.
	start := make(chan struct{})
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = adapters[i].AutoMigrate(ctx, reg)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d failed to migrate a schema its peers already fixed: %v", i, err)
		}
	}

	// The constraint must exist exactly once — tolerating the duplicate must not
	// have degraded into skipping the constraint altogether.
	rows, err := admin.Raw(ctx,
		`SELECT COUNT(*) FROM information_schema.table_constraints
		  WHERE table_schema = $1 AND constraint_type = 'FOREIGN KEY'
		    AND table_name = 'fk_race_books'`, schema).Rows()
	if err != nil {
		t.Fatalf("count constraints: %v", err)
	}
	defer rows.Close()
	var n int
	if !rows.Next() {
		t.Fatal("constraint count query returned no rows")
	}
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan constraint count: %v", err)
	}
	if n != 1 {
		t.Errorf("foreign keys on fk_race_books: got %d, want exactly 1 "+
			"(tolerating the duplicate must not skip the constraint entirely)", n)
	}
}

// The race test above only runs on the Postgres lane, so its fixture is
// unguarded everywhere else: if the relation tag ever stopped producing a
// database-enforced edge, that test would keep passing while testing nothing.
// This asserts the fixture on both lanes.
func TestAutoMigrate_FKRaceFixtureDeclaresADatabaseEnforcedForeignKey(t *testing.T) {
	t.Parallel()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(FKRaceAuthor{}, fkRaceAuthorConfig())
	srv.MustRegister(FKRaceBook{}, fkRaceBookConfig())

	reg := srv.Registry()
	book, ok := reg.Get("FKRaceBook")
	if !ok {
		t.Fatal("FKRaceBook is not registered")
	}
	fks := maniflex.ForeignKeysFor(reg, book)
	if len(fks) != 1 {
		t.Fatalf("database-enforced foreign keys on FKRaceBook: got %d, want 1 — "+
			"the Postgres race test would exercise no ADD CONSTRAINT at all", len(fks))
	}
}
