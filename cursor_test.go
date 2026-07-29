package maniflex

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"
)

func rawCursorToken(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func TestCursorToken_TimeUsesVersionedCanonicalRepresentation(t *testing.T) {
	t.Parallel()

	stamp := time.Date(2026, 7, 28, 12, 34, 56, 500_000_000, time.FixedZone("east", 3*60*60))
	token := EncodeCursor(stamp, "row-1")

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode token base64: %v", err)
	}
	var wire cursorToken
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode token JSON: %v", err)
	}
	if wire.Version != cursorTokenVersion {
		t.Errorf("version = %d, want %d", wire.Version, cursorTokenVersion)
	}
	if wire.Type != cursorTypeTime {
		t.Errorf("type = %q, want %q", wire.Type, cursorTypeTime)
	}
	if got, want := wire.V, CanonicalTime(stamp); got != want {
		t.Errorf("wire value = %v, want %q", got, want)
	}

	value, id, err := DecodeCursor(token)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if value != CanonicalTime(stamp) || id != "row-1" {
		t.Errorf("decoded = (%v, %q), want (%q, row-1)", value, id, CanonicalTime(stamp))
	}
}

func TestDecodeCursorForField_CanonicalizesLegacyTimeToken(t *testing.T) {
	t.Parallel()

	legacyJSON, err := json.Marshal(cursorToken{
		V:  "2026-07-28T12:34:56.5Z",
		ID: "legacy-row",
	})
	if err != nil {
		t.Fatalf("marshal legacy token: %v", err)
	}
	token := base64.RawURLEncoding.EncodeToString(legacyJSON)
	field := &FieldMeta{
		Type: reflect.TypeOf(time.Time{}),
		Tags: FieldTags{DBName: "created_at"},
	}

	value, id, err := decodeCursorForField(token, field)
	if err != nil {
		t.Fatalf("decodeCursorForField: %v", err)
	}
	if value != "2026-07-28T12:34:56.500000000Z" || id != "legacy-row" {
		t.Errorf("decoded = (%v, %q), want canonical timestamp and legacy-row", value, id)
	}
}

func TestDecodeCursorForField_RejectsTokenTypeMismatch(t *testing.T) {
	t.Parallel()

	field := &FieldMeta{
		Type: reflect.TypeOf(int(0)),
		Tags: FieldTags{DBName: "sequence"},
	}
	if _, _, err := decodeCursorForField(EncodeCursor("12", "row-1"), field); err == nil {
		t.Fatal("string token accepted for integer cursor field")
	}
}

func TestDecodeCursorRejectsInvalidWireValues(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"missing id":        `{"v":1}`,
		"empty id":          `{"v":1,"id":""}`,
		"missing value":     `{"id":"row-1"}`,
		"null value":        `{"v":null,"id":"row-1"}`,
		"object value":      `{"v":{"nested":true},"id":"row-1"}`,
		"array value":       `{"v":[1,2],"id":"row-1"}`,
		"trailing value":    `{"v":1,"id":"row-1"} true`,
		"unknown field":     `{"v":1,"id":"row-1","extra":true}`,
		"versioned json":    `{"version":1,"type":"json","v":"x","id":"row-1"}`,
		"fractional int":    `{"version":1,"type":"integer","v":1.5,"id":"row-1"}`,
		"overflowing float": `{"version":1,"type":"number","v":1e1000,"id":"row-1"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeCursor(rawCursorToken(payload)); err == nil {
				t.Fatalf("DecodeCursor accepted %s payload: %s", name, payload)
			}
		})
	}
}

func TestDecodeCursorAcceptsStrictLegacyScalars(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		payload string
		value   any
	}{
		{`{"v":"alpha","id":"row-s"}`, "alpha"},
		{`{"v":true,"id":"row-b"}`, true},
		{`{"v":42,"id":"row-i"}`, int64(42)},
		{`{"v":1.5,"id":"row-f"}`, float64(1.5)},
	} {
		got, _, err := DecodeCursor(rawCursorToken(tc.payload))
		if err != nil {
			t.Fatalf("DecodeCursor(%s): %v", tc.payload, err)
		}
		if !reflect.DeepEqual(got, tc.value) {
			t.Errorf("DecodeCursor(%s) value = %#v, want %#v", tc.payload, got, tc.value)
		}
	}
}

func TestDecodeCursorForFieldValidatesRangeAndKind(t *testing.T) {
	t.Parallel()

	uintField := &FieldMeta{
		Type: reflect.TypeOf(uint64(0)),
		Tags: FieldTags{DBName: "sequence"},
	}
	value, id, err := decodeCursorForField(EncodeCursor(uint64(math.MaxInt64), "row-u"), uintField)
	if err != nil {
		t.Fatalf("max database integer cursor: %v", err)
	}
	if value != uint64(math.MaxInt64) || id != "row-u" {
		t.Errorf("uint64 cursor = (%v, %q), want (%d, row-u)", value, id, uint64(math.MaxInt64))
	}
	if _, _, err := decodeCursorForField(
		EncodeCursor(uint64(math.MaxUint64), "row-too-large"),
		uintField,
	); err == nil {
		t.Fatal("uint64 value above database/sql's signed integer range was accepted")
	}

	intField := &FieldMeta{
		Type: reflect.TypeOf(int8(0)),
		Tags: FieldTags{DBName: "small_sequence"},
	}
	if _, _, err := decodeCursorForField(
		rawCursorToken(`{"v":128,"id":"row-overflow"}`),
		intField,
	); err == nil {
		t.Fatal("out-of-range legacy integer cursor was accepted for int8 field")
	}

	unsupportedField := &FieldMeta{
		Type: reflect.TypeOf([]string{}),
		Tags: FieldTags{DBName: "labels"},
	}
	if _, _, err := decodeCursorForField(
		rawCursorToken(`{"v":"x","id":"row-labels"}`),
		unsupportedField,
	); err == nil {
		t.Fatal("cursor was accepted for an unsupported slice field")
	}
}

func TestEncodeCursorRefusesIncompleteOrNonScalarKeys(t *testing.T) {
	t.Parallel()

	for name, token := range map[string]string{
		"empty id":     EncodeCursor(1, ""),
		"null value":   EncodeCursor(nil, "row-1"),
		"object value": EncodeCursor(struct{ N int }{N: 1}, "row-1"),
		"array value":  EncodeCursor([]int{1}, "row-1"),
	} {
		if token != "" {
			t.Errorf("%s produced token %q, want empty", name, token)
		}
	}
}
