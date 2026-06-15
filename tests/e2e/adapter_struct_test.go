package e2e

// Phase 3 / T3.2 PoD: drive the migrated DBAdapter directly through its typed
// interface — *T in, *T out — against a real database on both lanes. This
// exercises the adapter boundary independently of the HTTP pipeline (which the
// rest of the suite covers). Reuses the SpikeWide model from spike_roundtrip_test.

import (
	"context"
	"testing"

	"maniflex"
	"maniflex/pkg/money"
	"maniflex/tests/e2e/testutil"
)

// derefStr returns the string value whether v is a string or a *string (the
// adapter's typed read yields a *string for a *string field; the write bridge a
// plain string).
func derefStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case *string:
		if s != nil {
			return *s
		}
	}
	return ""
}

func TestAdapter_TypedCRUD_BothLanes(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SpikeWide{}}})
	adapter := srv.ManiflexServer().DB()
	meta := spikeMeta(t, srv)
	ctx := context.Background()

	// Create: build a record carrier from a DB-column map (the same path the
	// pipeline's DB step uses) and hand the *T to the typed Create.
	note := "typed-note"
	rec, err := maniflex.MapToRecord(meta, map[string]any{
		"id":     "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"name":   "Typed Jane",
		"age":    int64(41),
		"score":  12.5,
		"active": true,
		"price":  money.New(500, "USD"),
		"label":  `{"en":"Cardio"}`,
		"note":   note,
	})
	if err != nil {
		t.Fatalf("MapToRecord: %v", err)
	}
	created, err := adapter.Create(ctx, meta, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created == nil {
		t.Fatal("Create returned nil record")
	}
	cm := maniflex.RecordToMap(meta, created)
	if cm["name"] != "Typed Jane" || cm["id"] != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("create round-trip: %#v", cm)
	}

	// Read.
	got, err := adapter.FindByID(ctx, meta, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", &maniflex.QueryParams{Page: 1, Limit: 1})
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	gm := maniflex.RecordToMap(meta, got)
	if gm["name"] != "Typed Jane" {
		t.Errorf("read name = %v", gm["name"])
	}
	if derefStr(gm["note"]) != note {
		t.Errorf("read note = %v, want %q", gm["note"], note)
	}

	// Update only the present column (PATCH semantics).
	upd, _ := maniflex.MapToRecord(meta, map[string]any{"name": "Renamed"})
	updated, err := adapter.Update(ctx, meta, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", upd, map[string]struct{}{"name": {}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	um := maniflex.RecordToMap(meta, updated)
	if um["name"] != "Renamed" {
		t.Errorf("update name = %v, want Renamed", um["name"])
	}
	if derefStr(um["note"]) != note {
		t.Errorf("update clobbered note = %v, want %q (present-only)", um["note"], note)
	}

	// List returns []any of *T.
	items, total, err := adapter.FindMany(ctx, meta, &maniflex.QueryParams{Page: 1, Limit: 10})
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("FindMany: items=%d total=%d err=%v", len(items), total, err)
	}

	// Delete.
	if err := adapter.Delete(ctx, meta, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := adapter.FindByID(ctx, meta, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", &maniflex.QueryParams{}); err != maniflex.ErrNotFound {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
}
