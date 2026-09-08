package maniflex

// SetField type-mismatch handling (roadmap §10.6). When a SetField value cannot
// be represented in the target struct field's Go type, syncRecordField leaves
// the record's field untouched but clears the field's present-flag, so the write
// path deterministically falls back to ParsedBody (which SetField always wrote)
// instead of silently keeping the stale record value.

import (
	"reflect"
	"testing"
)

func mustScanCarrier(t *testing.T) *ModelMeta {
	t.Helper()
	meta, err := ScanModel(carrierModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	return meta
}

// A SetField whose value fits the field stays synced onto the record and keeps
// the present-flag — the baseline the mismatch case is contrasted against.
func TestSetField_CompatibleValueSyncsRecord(t *testing.T) {
	meta := mustScanCarrier(t)
	rec := &carrierModel{}
	ctx := &ServerContext{Model: meta, Record: rec}

	ctx.SetField("age", 7)

	if rec.Age != 7 {
		t.Errorf("record Age = %d, want 7 (compatible value mirrors onto the record)", rec.Age)
	}
	ageDB := meta.FieldByJSONName("age").Tags.DBName
	if _, ok := PresentColumns(rec)[ageDB]; !ok {
		t.Errorf("present-set %v missing %q after a compatible SetField", PresentColumns(rec), ageDB)
	}
	if v, _ := ctx.Field("age"); v != 7 {
		t.Errorf("ParsedBody age = %v, want 7", v)
	}
}

// A SetField overwriting an already-present key with a type-incompatible value
// must not be silently dropped: ParsedBody takes the new value, the record's
// field is left alone, and its present-flag is cleared so recordSourcedWrite
// fails and the write sources from ParsedBody.
func TestSetField_TypeMismatchClearsPresentFlag(t *testing.T) {
	meta := mustScanCarrier(t)
	ageDB := meta.FieldByJSONName("age").Tags.DBName

	// Simulate a bound record: age=1 came from the request body and is present.
	rec := &carrierModel{Age: 1}
	rec.mfxSetPresent(map[string]struct{}{ageDB: {}})
	ctx := &ServerContext{Model: meta, Record: rec}
	ctx.SetField("age", 1) // re-establish ParsedBody parity with the bound record

	if !recordSourcedWrite(ctx, meta) {
		t.Fatalf("precondition: record should source the write before the mismatch")
	}

	// A string can't be represented in the int field.
	ctx.SetField("age", "not-a-number")

	if rec.Age != 1 {
		t.Errorf("record Age = %d, want 1 (mismatch must leave the record value untouched)", rec.Age)
	}
	if _, ok := PresentColumns(rec)[ageDB]; ok {
		t.Errorf("present-set %v still has %q; the mismatch must clear the flag", PresentColumns(rec), ageDB)
	}
	if v, _ := ctx.Field("age"); v != "not-a-number" {
		t.Errorf("ParsedBody age = %v, want \"not-a-number\" (ParsedBody stays authoritative)", v)
	}
	if recordSourcedWrite(ctx, meta) {
		t.Errorf("recordSourcedWrite still true; the write must fall back to ParsedBody after the mismatch")
	}
	if got := toDBMap(ctx, ctx.ParsedBody, meta)[ageDB]; got != "not-a-number" {
		t.Errorf("write fallback age = %v, want \"not-a-number\" (SetField value, not the stale record value)", got)
	}
}

// A SetField with a type-incompatible value for a key that was ABSENT from the
// body still persists through the ParsedBody fallback (clearing an unset flag is
// a no-op), so it is never silently dropped either.
func TestSetField_TypeMismatchAbsentKeyPersistsViaParsedBody(t *testing.T) {
	meta := mustScanCarrier(t)
	ageDB := meta.FieldByJSONName("age").Tags.DBName

	rec := &carrierModel{}
	rec.mfxSetPresent(map[string]struct{}{}) // nothing bound from the body
	ctx := &ServerContext{Model: meta, Record: rec}

	ctx.SetField("age", "not-a-number") // new key, incompatible type

	if _, ok := PresentColumns(rec)[ageDB]; ok {
		t.Errorf("present-set %v unexpectedly gained %q", PresentColumns(rec), ageDB)
	}
	if recordSourcedWrite(ctx, meta) {
		t.Errorf("recordSourcedWrite true; body carries a key the record never bound")
	}
	if got := toDBMap(ctx, ctx.ParsedBody, meta)[ageDB]; got != "not-a-number" {
		t.Errorf("write fallback age = %v, want \"not-a-number\"", got)
	}
}

// ── SetField(name, nil) ───────────────────────────────────────────────────────
//
// Audit STEP-3 — syncRecordField returned early on a nil value, so the record's
// field kept whatever bindRecord had bound from the client's body while its
// present-flag stayed set. recordSourcedWrite then emitted the client's value
// for a key the server had just cleared: a middleware doing
// SetField("owner_id", nil) stored the id the client sent, where the map
// fallback would have written NULL.

type nilCarrier struct {
	BaseModel
	Name    string   `json:"name"`
	OwnerID *string  `json:"owner_id"`
	Tags    []string `json:"tags"`
}

func mustScanNilCarrier(t *testing.T) *ModelMeta {
	t.Helper()
	meta, err := ScanModel(nilCarrier{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	return meta
}

// The case with teeth: a pointer field the client populated, cleared by
// middleware. The record must end up nil and stay authoritative, so the write
// emits NULL rather than the client's value.
func TestSetField_NilClearsAPointerFieldOnTheRecord(t *testing.T) {
	meta := mustScanNilCarrier(t)
	ownerDB := meta.FieldByJSONName("owner_id").Tags.DBName

	clientValue := "someone-elses-id"
	rec := &nilCarrier{OwnerID: &clientValue}
	rec.mfxSetPresent(map[string]struct{}{ownerDB: {}})
	ctx := &ServerContext{Model: meta, Record: rec}
	ctx.SetField("owner_id", &clientValue) // ParsedBody parity with the bound record

	if !recordSourcedWrite(ctx, meta) {
		t.Fatal("precondition: the record should source the write before the clear")
	}

	ctx.SetField("owner_id", nil)

	if rec.OwnerID != nil {
		t.Errorf("record OwnerID = %q, want nil — the clear was dropped", *rec.OwnerID)
	}
	if _, ok := PresentColumns(rec)[ownerDB]; !ok {
		t.Errorf("present-set %v lost %q; a representable nil stays present so the "+
			"column is written as NULL rather than omitted", PresentColumns(rec), ownerDB)
	}
	if v, ok := ctx.Field("owner_id"); !ok || v != nil {
		t.Errorf("ParsedBody owner_id = %v, %v; want nil, true", v, ok)
	}
	// Whichever path the DB step takes, the stored value must be NULL. The record
	// path yields a typed nil pointer, which database/sql writes as NULL — hence
	// writesNull rather than a plain != nil, which a (*string)(nil) fails.
	if got := recordToMap(meta, rec)[ownerDB]; !writesNull(got) {
		t.Errorf("record write emits owner_id = %#v, want NULL — the client's value survived", got)
	}
	if got := toDBMap(ctx, ctx.ParsedBody, meta)[ownerDB]; !writesNull(got) {
		t.Errorf("fallback write emits owner_id = %#v, want NULL", got)
	}
}

// A slice is nilable too, and takes the same path as a pointer.
func TestSetField_NilClearsASliceFieldOnTheRecord(t *testing.T) {
	meta := mustScanNilCarrier(t)
	tagsDB := meta.FieldByJSONName("tags").Tags.DBName

	rec := &nilCarrier{Tags: []string{"from-the-client"}}
	rec.mfxSetPresent(map[string]struct{}{tagsDB: {}})
	ctx := &ServerContext{Model: meta, Record: rec}

	ctx.SetField("tags", nil)

	if rec.Tags != nil {
		t.Errorf("record Tags = %v, want nil", rec.Tags)
	}
	if _, ok := PresentColumns(rec)[tagsDB]; !ok {
		t.Errorf("present-set %v lost %q", PresentColumns(rec), tagsDB)
	}
}

// A non-nilable field has no nil to store, so this takes the type-mismatch
// remedy instead: drop the present-flag and let ParsedBody decide. The Validate
// step normally refuses such a body first, with a message naming the field.
func TestSetField_NilOnANonNilableFieldFallsBackToParsedBody(t *testing.T) {
	meta := mustScanNilCarrier(t)
	nameDB := meta.FieldByJSONName("name").Tags.DBName

	rec := &nilCarrier{Name: "from-the-client"}
	rec.mfxSetPresent(map[string]struct{}{nameDB: {}})
	ctx := &ServerContext{Model: meta, Record: rec}
	ctx.SetField("name", "from-the-client")

	ctx.SetField("name", nil)

	if rec.Name != "from-the-client" {
		t.Errorf("record Name = %q; a string cannot hold nil, so the field is left alone", rec.Name)
	}
	if _, ok := PresentColumns(rec)[nameDB]; ok {
		t.Errorf("present-set %v still has %q; without clearing it the record would "+
			"source the write and re-emit the client's value", PresentColumns(rec), nameDB)
	}
	if recordSourcedWrite(ctx, meta) {
		t.Error("recordSourcedWrite still true; the write must fall back to ParsedBody")
	}
	if got := toDBMap(ctx, ctx.ParsedBody, meta)[nameDB]; got != nil {
		t.Errorf("fallback write emits name = %#v, want nil", got)
	}
}

// Clearing a key the client never sent is not an error, and must not resurrect
// a value from the zero record.
func TestSetField_NilOnAnAbsentKey(t *testing.T) {
	meta := mustScanNilCarrier(t)
	ownerDB := meta.FieldByJSONName("owner_id").Tags.DBName

	rec := &nilCarrier{}
	ctx := &ServerContext{Model: meta, Record: rec}

	ctx.SetField("owner_id", nil)

	if rec.OwnerID != nil {
		t.Errorf("record OwnerID = %v, want nil", rec.OwnerID)
	}
	if _, ok := PresentColumns(rec)[ownerDB]; !ok {
		t.Errorf("present-set %v missing %q; an explicit nil is a value to write",
			PresentColumns(rec), ownerDB)
	}
}

// writesNull reports whether a value handed to the adapter stores as SQL NULL:
// an untyped nil, or a typed nil pointer such as the (*string)(nil) a cleared
// pointer field yields.
func writesNull(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return nilableKind(rv.Kind()) && rv.IsNil()
}
