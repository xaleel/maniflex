package e2e

// AutoMigrate adds missing columns and warns about extra ones, but a column
// whose Go type changed was neither detected nor reported: the old column
// silently stayed (int → string kept the INTEGER), so the schema drifted from
// the model with nothing in the logs to say so (BUG-16).

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// Two models over one table: qty starts as an int and becomes a string.
type driftWidgetV1 struct {
	maniflex.BaseModel
	Qty int `json:"qty" db:"qty"`
}

type driftWidgetV2 struct {
	maniflex.BaseModel
	Qty string `json:"qty" db:"qty"`
}

func driftServer(t *testing.T, model any) *maniflex.Server {
	t.Helper()
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	srv.MustRegister(model, maniflex.ModelConfig{TableName: "drift_widgets"})
	return srv
}

// captureWarnings swaps in a logger that records WARN and above, and returns
// what it collected.
func captureWarnings(t *testing.T, db maniflex.DBAdapter, migrate func()) string {
	t.Helper()
	setter, ok := db.(interface{ SetLogger(*slog.Logger) })
	if !ok {
		t.Fatalf("adapter %T does not accept a logger", db)
	}
	var logs bytes.Buffer
	setter.SetLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	migrate()
	return logs.String()
}

func TestAutoMigrate_WarnsOnColumnTypeChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	v1 := driftServer(t, driftWidgetV1{})
	db, err := sqlite.Open(":memory:", v1.Registry())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(ctx, v1.Registry()); err != nil {
		t.Fatalf("migrate v1: %v", err)
	}

	// Re-migrating the same model must stay silent — a warning that fires on a
	// table that matches its model would drown the real one.
	if out := captureWarnings(t, db, func() {
		if err := db.AutoMigrate(ctx, v1.Registry()); err != nil {
			t.Fatalf("re-migrate v1: %v", err)
		}
	}); out != "" {
		t.Errorf("re-migrating an unchanged model warned:\n%s", out)
	}

	// Now qty is a string. The INTEGER column stays (AutoMigrate never rewrites
	// one) — but it must no longer do so quietly.
	v2 := driftServer(t, driftWidgetV2{})
	out := captureWarnings(t, db, func() {
		if err := db.AutoMigrate(ctx, v2.Registry()); err != nil {
			t.Fatalf("migrate v2: %v", err)
		}
	})

	if !strings.Contains(out, "column type differs from the model") {
		t.Fatalf("a changed column type produced no warning; logs:\n%s", out)
	}
	for _, want := range []string{"table=drift_widgets", "column=qty", "db_type=INTEGER", "model_type=TEXT"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not report %q; logs:\n%s", want, out)
		}
	}
}
