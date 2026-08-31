package e2e

// AutoMigrate is on by default: a Config with no migrate flag set migrates on
// Start()/MigrateOnly(). (Previously the documented "Default: true" was a lie —
// the bool zero value was false, so a server built from a bare Config silently
// skipped migration and 500'd on the first query.)
//
// DisableAutoMigrate suppresses the *automatic* migration Start runs as a side
// effect of booting. It does not suppress MigrateOnly, which is the explicit,
// single-purpose call — see TestAutoMigrate_MigrateOnlyRunsEvenWhenDisabled for
// why the distinction is load-bearing.

import (
	"context"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

type amItem struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

func amModelConfig() maniflex.ModelConfig { return maniflex.ModelConfig{TableName: "am_items"} }

// amServer builds a server over an isolated in-memory SQLite database and
// reports whether the am_items table exists at any point.
func amServer(t *testing.T, cfg maniflex.Config) (*maniflex.Server, func() bool) {
	t.Helper()
	srv := maniflex.New(cfg)
	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetDB(db)
	srv.MustRegister(amItem{}, amModelConfig())

	return srv, func() bool {
		res := db.Raw(context.Background(),
			"INSERT INTO am_items (id, name) VALUES (?, ?)", "1", "a")
		_, err := res.RowsAffected()
		return err == nil
	}
}

func TestAutoMigrate_DefaultOn(t *testing.T) {
	t.Parallel()
	srv, tableExists := amServer(t, maniflex.Config{PathPrefix: "/api"}) // no migrate flag

	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("default config should migrate: %v", err)
	}
	if !tableExists() {
		t.Fatal("table should exist after default migrate")
	}
}

// DisableAutoMigrate is about the migration Start runs implicitly. Booting with
// it set must leave the schema untouched.
func TestAutoMigrate_DisableSkipsTheImplicitMigrationOnStart(t *testing.T) {
	t.Parallel()
	srv, tableExists := amServer(t, maniflex.Config{
		PathPrefix:         "/api",
		Port:               freePort(t),
		DisableAutoMigrate: true,
		ShutdownTimeout:    2 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := srv.StartWithContext(ctx); err != nil {
		t.Fatalf("StartWithContext: %v", err)
	}
	if tableExists() {
		t.Fatal("Start must not migrate when DisableAutoMigrate is set")
	}
}

// The flag says *Auto*. MigrateOnly is the explicit call — the one the
// init-container pattern in its own godoc is built on — so it migrates
// regardless.
//
// Without this, the framework's documented production posture was a silent
// no-op: ValidateProduction *requires* DisableAutoMigrate, so the init container
// ran MigrateOnly, altered nothing, returned nil, and exited 0. The deploy then
// proceeded to a server with no schema.
func TestAutoMigrate_MigrateOnlyRunsEvenWhenDisabled(t *testing.T) {
	t.Parallel()
	srv, tableExists := amServer(t, maniflex.Config{
		PathPrefix:         "/api",
		DisableAutoMigrate: true,
	})

	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("MigrateOnly: %v", err)
	}
	if !tableExists() {
		t.Fatal("MigrateOnly must migrate even when DisableAutoMigrate is set")
	}
}
