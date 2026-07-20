package maniflex

// RedactRecord strips the columns that must never leave the response path from
// a raw DB result: hidden, write-only and encrypted fields, plus the `_hmac`
// companion of an encrypted+unique column (a searchable digest of the plaintext,
// which has no business on the wire either).
//
// It exists because three subsystems serialize `ctx.DBResult` directly rather
// than through the response serializer, and `ctx.DBResult` is the map the DB
// step has already *decrypted*: versioning snapshots (audit MS-3), event
// payloads (EV-1), and the realtime hub, which forwards the event payload
// (RT-6). Each was leaking the plaintext of every encrypted column and the
// contents of every hidden and write-only field. One exclusion set closes all
// three, and — more to the point — keeps them from drifting apart again.
//
// The keys are left as **DB column names**, matching what `ctx.DBResult`
// already carries. This is deliberately not the response projection: locale
// resolution and `ctx.RedactResponseField` masking are decisions made for one
// requesting caller, and an event is durable, replayable, and read by
// subscribers who never made that request.
//
// v is accepted as `any` because `ctx.DBResult` is: a create or update leaves a
// `map[string]any`, a typed read leaves a `*T` (whose `json` tags would
// serialize a hidden field just as readily), and a list leaves a `*ListResult`.
// All three are handled; anything else is returned unchanged, since a value the
// framework did not produce has no model to redact against.
func RedactRecord(model *ModelMeta, v any) any {
	if v == nil || model == nil {
		return v
	}
	switch t := v.(type) {
	case map[string]any:
		return redactDBMap(model, t)
	case *ListResult:
		if t == nil {
			return t
		}
		items := make([]any, len(t.Items))
		for i, it := range t.Items {
			items[i] = RedactRecord(model, it)
		}
		return &ListResult{Items: items, Total: t.Total, Query: t.Query}
	default:
		// A typed record. recordToMap yields the same DB-keyed shape the map
		// path produces, so a subscriber sees one payload shape regardless of
		// whether the model happens to have encrypted fields (which is what
		// decides between the two branches in the DB step).
		if m := recordToMap(model, v); m != nil {
			return redactDBMap(model, m)
		}
		return v
	}
}

// redactDBMap returns a copy of row without the excluded columns. A copy, not an
// in-place delete: the caller's map is ctx.DBResult, which the Response step
// still has to serialize in full.
func redactDBMap(model *ModelMeta, row map[string]any) map[string]any {
	if row == nil {
		return nil
	}
	excluded := excludedDBNames(model)
	out := make(map[string]any, len(row))
	for k, v := range row {
		if !excluded[k] {
			out[k] = v
		}
	}
	return out
}
