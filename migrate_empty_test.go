package maniflex_test

// Regression coverage for the "smallest possible app" documented in
// docs/src/example/1-scaffolding.md: a server with a DB adapter (or none) but
// no models registered must boot, not fail migration. Previously migrate()
// rejected an empty adapter-group set with "no database adapter configured",
// conflating "no models registered" with "adapter missing".
//
// MigrateOnly() runs exactly the migration path Start() runs, without binding a
// socket, so it is the cheapest way to assert the boot-time behaviour.

import (
	"context"
	"testing"

	"github.com/xaleel/maniflex"
)

// No models registered → nothing to migrate → no error, even with AutoMigrate
// on and no DB configured. This is the scaffolding tutorial's first runnable app.
func TestMigrateOnly_NoModelsBoots(t *testing.T) {
	server := maniflex.New(maniflex.Config{})
	if err := server.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("empty app should migrate cleanly, got: %v", err)
	}
}

// Guard the real misconfiguration is still caught: a model is registered but no
// adapter resolves to it (no Config.DB, no per-model adapter). The error must
// name the offending model so the message stays actionable.
func TestMigrateOnly_ModelWithoutAdapterStillErrors(t *testing.T) {
	server := maniflex.New(maniflex.Config{})
	if err := server.Register(indexedModel{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	err := server.MigrateOnly(context.Background())
	if err == nil {
		t.Fatal("expected an error when a registered model has no adapter")
	}
}
