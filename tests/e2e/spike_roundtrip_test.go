package e2e

// Phase-0 spike PoD (T0.1 / T0.2): prove the reflection struct-scan + struct
// write builders round-trip every awkward type equal to the current map path,
// under BOTH driver lanes. Run:
//
//	go test ./tests/e2e/... -run TestSpike                       # sqlite
//	MANIFLEX_TEST_DB=postgres go test ./tests/e2e/... -run TestSpike
//
// The spike code lives in maniflex/tests/spike and is throwaway (moves into
// db/sqlcore in Phase 2).

import (
	"context"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/money"
	"github.com/xaleel/maniflex/tests/spike"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// SpikeWide carries one of each awkward type the scan layer must handle:
// money.Amount (sql.Scanner + driver.Valuer + SQLTyper), a LocaleString stored
// as JSON, a nullable pointer, plus the usual scalars and BaseModel timestamps.
type SpikeWide struct {
	maniflex.BaseModel
	Name   string                `json:"name"`
	Age    int                   `json:"age"`
	Score  float64               `json:"score"`
	Active bool                  `json:"active"`
	Price  money.Amount          `json:"price"`
	Label  maniflex.LocaleString `mfx:"locale" json:"label"`
	Note   *string               `json:"note"`
}

func spikeDriver(t *testing.T, srv *testutil.Server) maniflex.DriverType {
	t.Helper()
	d, ok := srv.ManiflexServer().DB().(interface {
		DriverType() maniflex.DriverType
	})
	if !ok {
		t.Fatal("adapter does not expose DriverType()")
	}
	return d.DriverType()
}

// spikeExec runs a non-SELECT statement through the adapter's Raw escape hatch
// and fails the test if the driver rejected it.
func spikeExec(t *testing.T, ctx context.Context, adapter maniflex.DBAdapter, q string, args []any) {
	t.Helper()
	if _, err := adapter.Raw(ctx, q, args...).RowsAffected(); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func spikeMeta(t *testing.T, srv *testutil.Server) *maniflex.ModelMeta {
	t.Helper()
	m, ok := srv.ManiflexServer().Registry().Get("SpikeWide")
	if !ok {
		t.Fatal("SpikeWide not registered")
	}
	return m
}

func TestSpike_RoundTrip_BothLanes(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SpikeWide{}}})
	adapter := srv.ManiflexServer().DB()
	meta := spikeMeta(t, srv)
	drv := spikeDriver(t, srv)
	ctx := context.Background()

	note := "bedside note"
	// Insert a fully-typed struct via the spike write builder (T0.2). This is a
	// true struct-path write → struct-path read round-trip; the HTTP/map path
	// can't carry typed values like money.Amount (it would store raw JSON),
	// which is precisely the gap the migration closes.
	src := &SpikeWide{
		Name:   "Jane Q. Researcher",
		Age:    34,
		Score:  87.5,
		Active: true,
		Price:  money.New(1234, "USD"),
		Label:  maniflex.LocaleString{"en": "Cardiology", "ar": "أمراض القلب"},
		Note:   &note,
	}
	id := "11111111-1111-1111-1111-111111111111"
	src.ID = id // explicit so we can find it again (BuildInsert keeps a set id)
	q, args := spike.BuildInsert(meta, src, drv)
	spikeExec(t, ctx, adapter, q, args)

	// A second row with the nullable column NULL and an empty label.
	src2 := &SpikeWide{Name: "No Note", Price: money.New(0, "USD")}
	src2.ID = "22222222-2222-2222-2222-222222222222"
	q2, args2 := spike.BuildInsert(meta, src2, drv)
	spikeExec(t, ctx, adapter, q2, args2)

	// ── struct path ──────────────────────────────────────────────────────────
	rows, err := adapter.Raw(ctx, "SELECT * FROM "+meta.TableName).Rows()
	if err != nil {
		t.Fatalf("raw select (struct): %v", err)
	}
	got, err := spike.ScanStruct(rows, meta, drv)
	rows.Close()
	if err != nil {
		t.Fatalf("ScanStruct: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ScanStruct returned %d rows, want 2", len(got))
	}

	// ── map path (control) ───────────────────────────────────────────────────
	rows2, err := adapter.Raw(ctx, "SELECT * FROM "+meta.TableName).Rows()
	if err != nil {
		t.Fatalf("raw select (map): %v", err)
	}
	ctrl, err := spike.ScanMap(rows2)
	rows2.Close()
	if err != nil {
		t.Fatalf("ScanMap: %v", err)
	}

	// Index both paths by id.
	structByID := map[string]*SpikeWide{}
	for _, v := range got {
		w := v.(*SpikeWide)
		structByID[w.ID] = w
	}
	mapByID := map[string]map[string]any{}
	for _, m := range ctrl {
		mapByID[m["id"].(string)] = m
	}

	w := structByID[id]
	if w == nil {
		t.Fatalf("row %s missing from struct scan", id)
	}

	// Awkward types round-trip to the inserted values.
	if w.Name != "Jane Q. Researcher" {
		t.Errorf("Name = %q", w.Name)
	}
	if w.Age != 34 {
		t.Errorf("Age = %d", w.Age)
	}
	if w.Score != 87.5 {
		t.Errorf("Score = %v", w.Score)
	}
	if !w.Active {
		t.Errorf("Active = %v, want true", w.Active)
	}
	if w.Price.Cents != 1234 { // Scan restores cents; currency is a separate column concern
		t.Errorf("Price.Cents = %d, want 1234", w.Price.Cents)
	}
	if w.Label["en"] != "Cardiology" || w.Label["ar"] != "أمراض القلب" {
		t.Errorf("Label = %#v", w.Label)
	}
	if w.Note == nil || *w.Note != note {
		t.Errorf("Note = %v, want %q", w.Note, note)
	}
	if w.CreatedAt.IsZero() || w.UpdatedAt.IsZero() {
		t.Errorf("timestamps not scanned: created=%v updated=%v", w.CreatedAt, w.UpdatedAt)
	}
	if w.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt not UTC: %v", w.CreatedAt.Location())
	}

	// Struct path agrees with the map control on the plain scalar columns.
	cm := mapByID[id]
	if got, want := w.Name, cm["name"].(string); got != want {
		t.Errorf("Name struct=%q map=%q", got, want)
	}
	if got := toInt(t, cm["age"]); int64(w.Age) != got {
		t.Errorf("Age struct=%d map=%d", w.Age, got)
	}

	// NULL pointer column round-trips to nil on the struct path.
	var noteNil *SpikeWide
	for _, v := range got {
		if x := v.(*SpikeWide); x.Name == "No Note" {
			noteNil = x
		}
	}
	if noteNil == nil {
		t.Fatal("No Note row missing")
	}
	if noteNil.Note != nil {
		t.Errorf("Note = %v, want nil for NULL column", *noteNil.Note)
	}
}

// toInt coerces a scanned numeric (int64 on SQLite, may differ on PG) to int64.
func toInt(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	default:
		t.Fatalf("age not an integer: %T", v)
		return 0
	}
}

func TestSpike_NullIntoNonPointer_Errors(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SpikeWide{}}})
	adapter := srv.ManiflexServer().DB()
	meta := spikeMeta(t, srv)
	drv := spikeDriver(t, srv)

	// age is a non-pointer int; a NULL must produce a clear error, not a panic.
	rows, err := adapter.Raw(context.Background(),
		spikeSelectLiteral(drv, "'x-id' AS id", "NULL AS age")).Rows()
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	defer rows.Close()
	_, err = spike.ScanStruct(rows, meta, drv)
	if err == nil {
		t.Fatal("expected error scanning NULL into non-pointer age, got nil")
	}
	if !containsAll(err.Error(), "NULL", "non-pointer") {
		t.Errorf("error not descriptive: %v", err)
	}
}

func TestSpike_UnmappedColumn_Skipped(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SpikeWide{}}})
	adapter := srv.ManiflexServer().DB()
	meta := spikeMeta(t, srv)
	drv := spikeDriver(t, srv)

	// extra_col has no struct field → must be discarded without error.
	rows, err := adapter.Raw(context.Background(),
		spikeSelectLiteral(drv, "'x-id' AS id", "'Jane' AS name", "7 AS extra_col")).Rows()
	if err != nil {
		t.Fatalf("raw: %v", err)
	}
	defer rows.Close()
	got, err := spike.ScanStruct(rows, meta, drv)
	if err != nil {
		t.Fatalf("ScanStruct: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row, got %d", len(got))
	}
	if w := got[0].(*SpikeWide); w.Name != "Jane" || w.ID != "x-id" {
		t.Errorf("unexpected scan: %#v", w)
	}
}

// spikeSelectLiteral builds a one-row literal SELECT that works on both drivers
// (SQLite allows a bare SELECT; Postgres too).
func spikeSelectLiteral(_ maniflex.DriverType, cols ...string) string {
	q := "SELECT "
	for i, c := range cols {
		if i > 0 {
			q += ", "
		}
		q += c
	}
	return q
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
