package maniflex

import "maps"

// RequestBody holds the parsed, JSON-keyed request body. It is read-only from
// outside the maniflex package: the only way to mutate it is ctx.SetField /
// ctx.DeleteField, which also keep the typed record (ctx.Record) in sync.
//
// This is deliberate. When ParsedBody was a bare map[string]any, middleware could
// write ctx.ParsedBody["k"] = v directly — bypassing the record sync — and the
// typed write path would then silently drop the change (it sources columns from
// the in-sync record when the present-key sets match). Wrapping the map makes
// that bypass a compile error: there is no exported way to set a key except
// through ctx.SetField.
//
// All methods are nil-safe, so ctx.ParsedBody can be compared to nil (no body on
// the request) and still be read without a guard.
type RequestBody struct {
	m map[string]any
}

// NewRequestBody wraps a JSON-keyed map as a RequestBody. The pipeline builds one
// during Deserialize; this constructor is for callers (mainly tests) that build a
// ServerContext directly.
func NewRequestBody(m map[string]any) *RequestBody {
	if m == nil {
		m = make(map[string]any)
	}
	return &RequestBody{m: m}
}

// Get returns the value for a JSON key and whether the key was present (an
// explicit null is present with a nil value).
func (b *RequestBody) Get(key string) (any, bool) {
	if b == nil || b.m == nil {
		return nil, false
	}
	v, ok := b.m[key]
	return v, ok
}

// Has reports whether a JSON key is present, regardless of its value.
func (b *RequestBody) Has(key string) bool {
	if b == nil {
		return false
	}
	_, ok := b.m[key]
	return ok
}

// Len returns the number of top-level keys in the body.
func (b *RequestBody) Len() int {
	if b == nil {
		return 0
	}
	return len(b.m)
}

// Keys returns the top-level JSON keys in unspecified order.
func (b *RequestBody) Keys() []string {
	if b == nil {
		return nil
	}
	keys := make([]string, 0, len(b.m))
	for k := range b.m {
		keys = append(keys, k)
	}
	return keys
}

// Map returns a shallow copy of the body as a plain map, for read-only consumers
// such as validation callbacks and ABAC policies. Mutating the returned map does
// NOT change the body — use ctx.SetField / ctx.DeleteField for that.
func (b *RequestBody) Map() map[string]any {
	out := make(map[string]any, b.Len())
	if b != nil && b.m != nil {
		maps.Copy(out, b.m)
	}
	return out
}

// raw returns the live backing map for same-package hot paths (e.g. toDBMap) that
// only read it. Unexported so external code cannot reach the mutable map.
func (b *RequestBody) raw() map[string]any {
	if b == nil {
		return nil
	}
	return b.m
}

// set / del are the only mutators. They are unexported and called solely by
// ctx.SetField / ctx.DeleteField, which also sync the typed record — so the body
// and the record cannot drift apart for a present key.
func (b *RequestBody) set(key string, v any) {
	if b.m == nil {
		b.m = make(map[string]any)
	}
	b.m[key] = v
}

func (b *RequestBody) del(key string) {
	if b == nil {
		return
	}
	delete(b.m, key)
}
