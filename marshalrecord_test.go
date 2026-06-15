package maniflex

// Phase 4 / T4.2 gate: marshalRecord(record) must be byte-equivalent to the
// existing toJSONMap(recordToMap(record)) path. We verify both the bridge record
// (values on the extra carrier — exact map equality) and a field-populated
// record (values in struct fields — JSON-bytes equality, since native int vs
// driver int64 differ in Go type but not in JSON).

import (
	"reflect"
	"testing"
)

func TestMarshalRecord_EquivalentToJSONMap_Bridge(t *testing.T) {
	s := newDefaultSteps(nil, nil)
	meta, row := buildBenchWide(t)

	rec, err := mapToRecord(meta, row)
	if err != nil {
		t.Fatalf("mapToRecord: %v", err)
	}
	got := s.marshalRecord(meta, rec, nil)
	want := s.toJSONMap(row, meta, nil)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("marshalRecord != toJSONMap\n got=%#v\nwant=%#v", got, want)
	}
}

func TestMarshalRecord_EquivalentToJSONMap_Locale(t *testing.T) {
	s := newDefaultSteps(nil, nil)
	meta, err := ScanModel(benchLocaleModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	row := map[string]any{
		"id":         "550e8400-e29b-41d4-a716-446655440000",
		"created_at": "2026-05-29T10:00:00Z",
		"updated_at": "2026-05-29T10:00:00Z",
		"name":       `{"en":"Cardiology","ar":"أمراض القلب"}`,
		"code":       "CARD",
	}
	for _, ctx := range []*ServerContext{
		nil,
		{Locale: "en", DefaultLocale: "en"},
		{Locale: "ar", DefaultLocale: "en"},
	} {
		rec, _ := mapToRecord(meta, row)
		got := s.marshalRecord(meta, rec, ctx)
		want := s.toJSONMap(row, meta, ctx)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("locale ctx=%v: marshalRecord != toJSONMap\n got=%#v\nwant=%#v", ctx, got, want)
		}
	}
}

