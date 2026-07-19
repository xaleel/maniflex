package maniflex

// SetField type-mismatch handling (roadmap §10.6). When a SetField value cannot
// be represented in the target struct field's Go type, syncRecordField leaves
// the record's field untouched but clears the field's present-flag, so the write
// path deterministically falls back to ParsedBody (which SetField always wrote)
// instead of silently keeping the stale record value.

import "testing"

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
