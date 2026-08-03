package maniflex

// WR-1 — the bridge fills a nil pointer field from the value parked on the extra
// carrier, and the map round-trip it was built to protect is unchanged.
//
// The two halves are inseparable. assignField refuses anything but an exact type
// match, which is what makes recordToMap(mapToRecord(m)) == m hold for value
// types as well as keys — so a *string field never took the string the map held,
// and a record handed back from Create reported nil for a column that was written
// correctly. Filling the field is safe only for as long as it stays additive:
// the value must remain on the carrier, because every consumer of a bridge
// record reads the carrier in preference to the struct field.
//
//	go test . -run TestBridge

import (
	"reflect"
	"testing"
	"time"
)

type bridgeDoc struct {
	BaseModel
	Title       string     `json:"title"        db:"title"`
	Note        *string    `json:"note"         db:"note"`
	PublishedAt *time.Time `json:"published_at" db:"published_at"`
	Rank        *int       `json:"rank"         db:"rank"`
	Plain       int        `json:"plain"        db:"plain"`
	Small       int32      `json:"small"        db:"small"`
}

func bridgeMeta(t *testing.T) *ModelMeta {
	t.Helper()
	meta, err := ScanModel(bridgeDoc{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	return meta
}

// The WR-1 symptom itself: a value the map carried must be readable from the
// struct field, not only from the carrier.
func TestBridge_PointerFieldsAreHydrated(t *testing.T) {
	meta := bridgeMeta(t)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	rec, err := mapToRecord(meta, map[string]any{
		"title":        "t",
		"note":         "hello",
		"published_at": when,
		"rank":         7,
	})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)

	if doc.Note == nil || *doc.Note != "hello" {
		t.Errorf("Note = %v, want a pointer to %q", doc.Note, "hello")
	}
	if doc.PublishedAt == nil || !doc.PublishedAt.Equal(when) {
		t.Errorf("PublishedAt = %v, want a pointer to %v", doc.PublishedAt, when)
	}
	if doc.Rank == nil || *doc.Rank != 7 {
		t.Errorf("Rank = %v, want a pointer to 7", doc.Rank)
	}
	// A directly-assignable value never needed the carrier and is unaffected.
	if doc.Title != "t" {
		t.Errorf("Title = %q, want %q", doc.Title, "t")
	}
}

// The invariant the exact-type rule exists for. Hydration is additive — the value
// stays on the carrier — so the map that comes back is identical to the one that
// went in, key for key and Go type for Go type. Anything else would, for one,
// defeat the adapter's time.Time→RFC3339 normalisation on write.
func TestBridge_RoundTripIsUnchanged(t *testing.T) {
	meta := bridgeMeta(t)
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	in := map[string]any{
		"title":        "t",
		"note":         "hello",
		"published_at": when,
		"rank":         7,
		"plain":        3,
	}
	rec, err := mapToRecord(meta, in)
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	out := recordToMap(meta, rec)

	if len(out) != len(in) {
		t.Fatalf("round trip changed the key set: got %v, want %v", keysOf(out), keysOf(in))
	}
	for k, want := range in {
		got, ok := out[k]
		if !ok {
			t.Errorf("key %q was dropped", k)
			continue
		}
		if reflect.TypeOf(got) != reflect.TypeOf(want) {
			t.Errorf("key %q changed Go type: %T, want %T", k, got, want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("key %q = %#v, want %#v", k, got, want)
		}
	}
}

// A SQL NULL must stay nil. The column is present and the map holds nil, so the
// carrier holds nil — there is nothing to point at, and inventing a pointer to a
// zero value would turn "no value" into "the empty string".
func TestBridge_NullStaysNil(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{"title": "t", "note": nil})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)
	if doc.Note != nil {
		t.Errorf("Note = %v, want nil for a SQL NULL", *doc.Note)
	}
	out := recordToMap(meta, rec)
	if v, ok := out["note"]; !ok || v != nil {
		t.Errorf(`out["note"] = %#v (present=%v), want a present nil`, v, ok)
	}
}

// A driver-shaped integer is taken, because the echo map is a re-read of the
// row: a plain int column arrives as int64 and missed the exact-type test for
// the same reason a *string did. The conversion says the same number, which is
// the whole test — and the carrier keeps the int64, so the map is unchanged.
func TestBridge_DriverShapedNumbersAreTaken(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{
		"rank":  int64(7), // *int  field, driver-shaped
		"plain": int64(3), // int   field, driver-shaped
	})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)
	if doc.Rank == nil || *doc.Rank != 7 {
		t.Errorf("Rank = %v, want a pointer to 7", doc.Rank)
	}
	if doc.Plain != 3 {
		t.Errorf("Plain = %d, want 3", doc.Plain)
	}
	out := recordToMap(meta, rec)
	if out["rank"] != int64(7) || out["plain"] != int64(3) {
		t.Errorf("the carrier's Go types changed: rank=%#v plain=%#v", out["rank"], out["plain"])
	}
}

// Conversion stops where it would change what the value says. A string spelling
// a timestamp is not a time.Time — parsing it here would be scan logic in the
// wrong layer — and numeric↔string is excluded outright, since Go reads int→string
// as a rune and would turn 65 into "A".
func TestBridge_UnrelatedTypeIsNotCoerced(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{
		"published_at": "2026-01-02T03:04:05Z", // a string, not a time.Time
		"rank":         "7",                    // a string, not a number
	})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)
	if doc.PublishedAt != nil {
		t.Errorf("PublishedAt = %v, want nil — a string must not be parsed here", doc.PublishedAt)
	}
	if doc.Rank != nil {
		t.Errorf("Rank = %v, want nil — a string must not become a number here", doc.Rank)
	}
	// Both still ride the carrier, so nothing is lost.
	out := recordToMap(meta, rec)
	if out["published_at"] != "2026-01-02T03:04:05Z" {
		t.Errorf(`out["published_at"] = %#v`, out["published_at"])
	}
	if out["rank"] != "7" {
		t.Errorf(`out["rank"] = %#v`, out["rank"])
	}
}

// A number that does not survive the round trip is declined rather than
// silently wrapped or truncated. Reporting a wrong number is worse than
// reporting none, because only the second is visible.
func TestBridge_LossyNumberIsDeclined(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{
		"small": int64(1) << 40, // overflows the int32 column
		"rank":  1.5,            // a fraction cannot land in an int
	})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)
	if doc.Small != 0 {
		t.Errorf("Small = %d, want 0 — the value does not fit and must not wrap", doc.Small)
	}
	if doc.Rank != nil {
		t.Errorf("Rank = %v, want nil — 1.5 is not an int", doc.Rank)
	}
	out := recordToMap(meta, rec)
	if out["small"] != int64(1)<<40 || out["rank"] != 1.5 {
		t.Errorf("the carrier lost the original: small=%#v rank=%#v", out["small"], out["rank"])
	}
}

// A column absent from the map must leave its field alone, so an update that
// patches one column does not report the others as anything.
func TestBridge_AbsentColumnIsUntouched(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{"title": "t"})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	doc := rec.(*bridgeDoc)
	if doc.Note != nil || doc.PublishedAt != nil || doc.Rank != nil {
		t.Errorf("an absent column was materialised: Note=%v PublishedAt=%v Rank=%v",
			doc.Note, doc.PublishedAt, doc.Rank)
	}
	out := recordToMap(meta, rec)
	if _, ok := out["note"]; ok {
		t.Error(`out["note"] exists though the column was never present`)
	}
}

// The carrier must keep the value, because every reader of a bridge record
// prefers it: recordValue reads extra before the struct field, recordData goes
// through recordToMap where extra overwrites last, and structForWrite refuses
// the typed write path whenever extra is non-empty. Emptying it here would
// change all three.
func TestBridge_HydrationLeavesTheCarrierIntact(t *testing.T) {
	meta := bridgeMeta(t)
	rec, err := mapToRecord(meta, map[string]any{"note": "hello"})
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	extra := ExtraColumns(rec)
	if v, ok := extra["note"]; !ok || v != "hello" {
		t.Errorf(`the carrier lost "note": %#v (present=%v)`, v, ok)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
