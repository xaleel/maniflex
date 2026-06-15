package maniflex

// typed_body.go — request-body field access that stays consistent across the
// transition (typed-models migration, Phase 5 / T5.4). Middleware that inject or
// transform body fields (owner, tenant, hashing, type coercion) should use
// SetField instead of writing ctx.ParsedBody directly: it writes through to BOTH
// ctx.ParsedBody (today's write source) and the typed ctx.Record (so For[T] and
// the eventual struct-native write path see the same value). Field is the
// matching reader. This decouples middleware from the map representation, which
// is what lets the write path move from ParsedBody to ctx.Record later.

import "reflect"

// SetField sets a request-body field by its JSON name, writing through to
// ctx.ParsedBody and (when a typed record is bound) the matching struct field on
// ctx.Record, marking it present. Use it from create/update middleware.
func (c *ServerContext) SetField(jsonName string, value any) {
	if c.ParsedBody == nil {
		c.ParsedBody = NewRequestBody(nil)
	}
	c.ParsedBody.set(jsonName, value)
	c.syncRecordField(jsonName, value)
}

// Field reads a request-body field by its JSON name. ParsedBody is the
// authoritative body during the transition, so it is read from there.
func (c *ServerContext) Field(jsonName string) (any, bool) {
	return c.ParsedBody.Get(jsonName)
}

// DeleteField removes a request-body field by its JSON name from ParsedBody and
// clears it from the typed record's present set, so the write path skips it.
// Use it from middleware that strip a field before the DB step.
func (c *ServerContext) DeleteField(jsonName string) {
	c.ParsedBody.del(jsonName)
	if c.Model == nil {
		return
	}
	if f := c.Model.FieldByJSONName(jsonName); f != nil {
		if rm, ok := c.Record.(recordMeta); ok {
			if p := rm.mfxPresent(); p != nil {
				delete(p, f.Tags.DBName)
			}
		}
	}
}

// syncRecordField mirrors a SetField value onto the typed record when one is
// bound and the value fits the field's type. When the value cannot be
// represented in the field's Go type, the record's field is left alone but its
// present-flag is cleared, so ParsedBody (which SetField always wrote) stays
// authoritative for the column and the write path does not source a stale
// record value — see the type-mismatch note below.
func (c *ServerContext) syncRecordField(jsonName string, value any) {
	if c.Record == nil || c.Model == nil || value == nil {
		return
	}
	f := c.Model.FieldByJSONName(jsonName)
	if f == nil {
		return
	}
	fv := reflect.ValueOf(c.Record).Elem().FieldByIndex(f.Index)
	if !fv.CanSet() {
		return
	}
	rv := reflect.ValueOf(value)
	switch {
	case rv.Type().AssignableTo(fv.Type()):
		fv.Set(rv)
	case numericKind(fv.Kind()) && numericKind(rv.Kind()) && rv.Type().ConvertibleTo(fv.Type()):
		fv.Set(rv.Convert(fv.Type()))
	default:
		// The value can't be represented in the field's Go type. Leaving the
		// record's field untouched while keeping its present-flag set would let
		// recordSourcedWrite source the *old* record value for an already-present
		// key, silently dropping this SetField. Clear the present-flag instead:
		// the record's present-set no longer matches the body keys, so the DB step
		// falls back to toDBMap(ParsedBody) and the SetField value (written above)
		// wins deterministically. (A new key was never present, so this is a no-op
		// for it and the ParsedBody fallback already carried the value.)
		if rm, ok := c.Record.(recordMeta); ok {
			if p := rm.mfxPresent(); p != nil {
				delete(p, f.Tags.DBName)
			}
		}
		return
	}
	if rm, ok := c.Record.(recordMeta); ok {
		p := rm.mfxPresent()
		if p == nil {
			p = make(map[string]struct{})
			rm.mfxSetPresent(p)
		}
		p[f.Tags.DBName] = struct{}{}
	}
}

func numericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
