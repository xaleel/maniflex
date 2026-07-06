package e2e

// AutoMigrate is now on by default: a Config with no migrate flag set migrates on
// Start()/MigrateOnly(). Opt out with DisableAutoMigrate. (Previously the
// documented "Default: true" was a lie — the bool zero value was false, so a
// server built from a bare Config silently skipped migration and 500'd on the
// first query.)

import (
	"context"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

type amItem struct {
	maniflex.BaseModel
	Name string `json:"name"`
}

func amModelConfig() maniflex.ModelConfig { return maniflex.ModelConfig{TableName: "am_items"} }

func TestAutoMigrate_DefaultOn(t *testing.T) {
	t.Parallel()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api"}) // no migrate flag
	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetDB(db)
	srv.MustRegister(amItem{}, amModelConfig())

	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("default config should migrate: %v", err)
	}
	res := db.Raw(context.Background(), "INSERT INTO am_items (id, name) VALUES (?, ?)", "1", "a")
	if _, err := res.RowsAffected(); err != nil {
		t.Fatalf("table should exist after default migrate: %v", err)
	}
}

func TestAutoMigrate_DisableSkips(t *testing.T) {
	t.Parallel()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv.SetDB(db)
	srv.MustRegister(amItem{}, amModelConfig())

	if err := srv.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("MigrateOnly should be a clean no-op when disabled: %v", err)
	}
	res := db.Raw(context.Background(), "INSERT INTO am_items (id, name) VALUES (?, ?)", "1", "a")
	if _, err := res.RowsAffected(); err == nil {
		t.Fatal("table must NOT exist when DisableAutoMigrate is set")
	}
}
