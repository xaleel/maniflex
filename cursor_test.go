package maniflex

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

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
