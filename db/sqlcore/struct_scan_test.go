package sqlcore

// Phase 2 PoDs (T2.1 scan, T2.2 write, T2.3 bridge). The core module has no SQL
// driver, so the scanner is exercised through its DB-free seam (scanStructValues)
// with driver-shaped values for both lanes — the *sql.Rows path shares the same
// cached plan and is covered end-to-end by the real-driver e2e suite in Phase 3.

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"maniflex"
	"maniflex/pkg/money"
)

type wideRow struct {
	maniflex.BaseModel
	Name   string                `json:"name"`
	Age    int                   `json:"age"`
	Score  float64               `json:"score"`
	Active bool                  `json:"active"`
	Price  money.Amount          `json:"price"`
	Label  maniflex.LocaleString `mfx:"locale" json:"label"`
	Note   *string               `json:"note"`
}

func wideMeta(t testing.TB) *maniflex.ModelMeta {
	t.Helper()
	m, err := maniflex.ScanModel(wideRow{}, maniflex.ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	return m
}

var wideCols = []string{"id", "created_at", "updated_at", "name", "age", "score", "active", "price", "label", "note"}

// sqliteRow returns a row shaped the way modernc/sqlite delivers it: TEXT
// timestamps, INTEGER (int64) ints and 0/1 bools, TEXT numeric/JSON.
func sqliteRow(note any) []any {
	return []any{
		"11111111-1111-1111-1111-111111111111",
		"2026-05-30T10:00:00Z", "2026-05-30T10:05:00Z",
		"Jane", int64(34), 87.5, int64(1),
		"12.3400", `{"en":"Cardiology","ar":"أمراض القلب"}`, note,
	}
}

// pgRow returns a row shaped the way lib/pq delivers it: native time.Time, bool,
// NUMERIC as []byte.
func pgRow(note any) []any {
	ts, _ := time.Parse(time.RFC3339, "2026-05-30T10:00:00Z")
	return []any{
		"11111111-1111-1111-1111-111111111111",
		ts, ts,
		"Jane", int64(34), 87.5, true,
		[]byte("12.3400"), `{"en":"Cardiology","ar":"أمراض القلب"}`, note,
	}
}

func assertWide(t *testing.T, v any, wantNote *string) {
	t.Helper()
	w, ok := v.(*wideRow)
	if !ok {
		t.Fatalf("got %T, want *wideRow", v)
	}
	if w.ID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ID = %q", w.ID)
	}
	if w.Name != "Jane" || w.Age != 34 || w.Score != 87.5 || !w.Active {
		t.Errorf("scalars wrong: %+v", w)
	}
	if w.Price.Cents != 1234 {
		t.Errorf("Price.Cents = %d, want 1234", w.Price.Cents)
	}
	if w.Label["en"] != "Cardiology" || w.Label["ar"] != "أمراض القلب" {
		t.Errorf("Label = %#v", w.Label)
	}
	if w.CreatedAt.IsZero() || w.UpdatedAt.IsZero() {
		t.Errorf("timestamps not scanned: %v / %v", w.CreatedAt, w.UpdatedAt)
	}
	switch {
	case wantNote == nil && w.Note != nil:
		t.Errorf("Note = %v, want nil", *w.Note)
	case wantNote != nil && (w.Note == nil || *w.Note != *wantNote):
		t.Errorf("Note = %v, want %q", w.Note, *wantNote)
	}
}

func TestScanStruct_BothDriverShapes(t *testing.T) {
	meta := wideMeta(t)
	note := "bedside note"

	t.Run("sqlite shape", func(t *testing.T) {
		a := &Adapter{driver: maniflex.SQLite}
		v, err := a.scanStructValues(meta, wideCols, sqliteRow(note))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		assertWide(t, v, &note)

		vNil, err := a.scanStructValues(meta, wideCols, sqliteRow(nil))
		if err != nil {
			t.Fatalf("scan nil note: %v", err)
		}
		assertWide(t, vNil, nil)
	})

	t.Run("postgres shape", func(t *testing.T) {
		a := &Adapter{driver: maniflex.Postgres}
		v, err := a.scanStructValues(meta, wideCols, pgRow(note))
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		assertWide(t, v, &note)
	})
}

func TestScanStruct_NullIntoNonPointer(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}
	meta := wideMeta(t)
	_, err := a.scanStructValues(meta, []string{"id", "age"}, []any{"x", nil})
	if err == nil {
		t.Fatal("expected error for NULL into non-pointer age")
	}
	if !strings.Contains(err.Error(), "NULL") || !strings.Contains(err.Error(), "non-pointer") {
		t.Errorf("error not descriptive: %v", err)
	}
}

func TestScanStruct_UnmappedColumnSkipped(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}
	meta := wideMeta(t)
	v, err := a.scanStructValues(meta, []string{"id", "extra_col"}, []any{"x", int64(7)})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if w := v.(*wideRow); w.ID != "x" {
		t.Errorf("ID = %q, want x", w.ID)
	}
}

func TestScanPlan_Cached(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}
	meta := wideMeta(t)
	p1 := a.scanPlanFor(meta, wideCols)
	p2 := a.scanPlanFor(meta, wideCols)
	// Same backing array → the plan was cached, not rebuilt.
	if reflect.ValueOf(p1).Pointer() != reflect.ValueOf(p2).Pointer() {
		t.Error("scanPlanFor did not return the cached plan")
	}
}

// TestScanStruct_Concurrent exercises concurrent cache access + scanning of the
// same model. Race conditions are caught under -race (CI); locally it at least
// proves no shared-holder corruption or panic.
func TestScanStruct_Concurrent(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}
	meta := wideMeta(t)
	note := "n"
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := a.scanStructValues(meta, wideCols, sqliteRow(note))
			if err != nil {
				t.Errorf("scan: %v", err)
				return
			}
			if w := v.(*wideRow); w.Age != 34 || w.Price.Cents != 1234 {
				t.Errorf("corrupt scan: %+v", w)
			}
		}()
	}
	wg.Wait()
}

func TestBuildInsert(t *testing.T) {
	meta := wideMeta(t)
	note := "hi"
	row := &wideRow{Name: "Jane", Age: 34, Score: 87.5, Active: true,
		Price: money.New(1234, "USD"), Note: &note}
	row.ID = "fixed-id"

	t.Run("sqlite", func(t *testing.T) {
		a := &Adapter{driver: maniflex.SQLite}
		q, args := a.buildInsert(meta, row, nil)
		if !strings.HasPrefix(q, `INSERT INTO "wide_rows" (`) {
			t.Errorf("query = %q", q)
		}
		if strings.Contains(q, "$1") || !strings.Contains(q, "?") {
			t.Errorf("sqlite should use ? placeholders: %q", q)
		}
		if len(args) != len(meta.Fields) {
			t.Errorf("args = %d, want %d", len(args), len(meta.Fields))
		}
	})

	t.Run("postgres", func(t *testing.T) {
		a := &Adapter{driver: maniflex.Postgres}
		q, _ := a.buildInsert(meta, row, nil)
		if !strings.Contains(q, "$1") {
			t.Errorf("postgres should use $N placeholders: %q", q)
		}
	})
}

func TestBuildUpdate_PresentOnly(t *testing.T) {
	a := &Adapter{driver: maniflex.SQLite}
	meta := wideMeta(t)
	row := &wideRow{Name: "Jane", Age: 99}
	present := map[string]struct{}{"name": {}} // JSON name

	q, args := a.buildUpdate(meta, "the-id", row, present)
	if !strings.Contains(q, `"name" = ?`) {
		t.Errorf("update should set name: %q", q)
	}
	if strings.Contains(q, `"age" = `) {
		t.Errorf("update must not set absent column age: %q", q)
	}
	if !strings.Contains(q, `"updated_at" = ?`) {
		t.Errorf("update should always set updated_at: %q", q)
	}
	if !strings.HasSuffix(q, `WHERE "id" = ?`) {
		t.Errorf("update WHERE wrong: %q", q)
	}
	// args: name, updated_at, id.
	if len(args) != 3 {
		t.Errorf("args = %v, want 3 (name, updated_at, id)", args)
	}
	if args[0] != "Jane" {
		t.Errorf("args[0] = %v, want Jane", args[0])
	}
}
