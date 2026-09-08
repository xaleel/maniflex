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
//
// A nil value is a value, not an absence: it clears the field and the column is
// written as NULL, which is how a middleware discards whatever the client sent
// for it. Use DeleteField to leave the column alone instead.
//
//	ctx.SetField("owner_id", nil)  // stored as NULL
//	ctx.DeleteField("owner_id")    // not written at all
//
// Clearing a field whose Go type cannot hold nil — a string, an int — leaves the
// record untouched and lets the body decide, which the Validate step then
// refuses with a message naming the field. Make the field a pointer to allow
// null.
func (c *ServerContext) SetField(jsonName string, value any) {
	if c.ParsedBody == nil {
		c.ParsedBody = NewRequestBody(nil)
	}
	// Remember that the server put this here, not the client. readonly and
	// immutable mean "not from a client" — a middleware stamping a tenant or an
	// owner is not a client — so the Validate step must not strip it back out.
	// Only middleware running before Validate could be affected, which until
	// ProvidesScope() hoisted scope providers was a narrow set.
	if c.serverSet == nil {
		c.serverSet = make(map[string]struct{}, 2)
	}
	c.serverSet[jsonName] = struct{}{}

	c.ParsedBody.set(jsonName, value)
	c.syncRecordField(jsonName, value)
}

// ServerSetField reports whether jsonName was written by SetField — that is, by
// the server rather than parsed from the request body.
func (c *ServerContext) ServerSetField(jsonName string) bool {
	_, ok := c.serverSet[jsonName]
	return ok
}

// Field reads a request-body field by its JSON name. ParsedBody is the
// authoritative body during the transition, so it is read from there.
func (c *ServerContext) Field(jsonName string) (any, bool) {
	return c.ParsedBody.Get(jsonName)
}

// DeleteField removes a request-body field by its JSON name from ParsedBody,
// clears it from the typed record's present set so the write path skips it, and
// resets the record's struct field to its zero value. Use it from middleware
// that strip a field before the DB step.
//
// The struct field is reset because the Deserialize step decodes the whole
// client body into ctx.Record before Validate runs — including keys Validate is
// about to strip, and matching them case-insensitively, so {"ROLE":"admin"}
// lands in a json:"role" field. Clearing only the present set left the written
// row correct while For, Bind, Handle and ctx.Record still handed application
// middleware the client's Role, Balance or TenantID, with nothing to mark them
// as refused — and Create[T], which deliberately trusts its caller rather than
// re-applying readonly, would persist them (audit STEP-4).
func (c *ServerContext) DeleteField(jsonName string) {
	c.ParsedBody.del(jsonName)
	if c.Model == nil {
		return
	}
	f := c.Model.FieldByJSONName(jsonName)
	if f == nil {
		return
	}
	if rm, ok := c.Record.(recordMeta); ok {
		if p := rm.mfxPresent(); p != nil {
			delete(p, f.Tags.DBName)
		}
	}
	if c.Record == nil {
		return
	}
	fv := reflect.ValueOf(c.Record).Elem().FieldByIndex(f.Index)
	if fv.CanSet() {
		fv.Set(reflect.Zero(fv.Type()))
	}
}

// syncRecordField mirrors a SetField value onto the typed record when one is
// bound and the value fits the field's type. When the value cannot be
// represented in the field's Go type, the record's field is left alone but its
// present-flag is cleared, so ParsedBody (which SetField always wrote) stays
// authoritative for the column and the write path does not source a stale
// record value — see the type-mismatch note below.
func (c *ServerContext) syncRecordField(jsonName string, value any) {
	if c.Record == nil || c.Model == nil {
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

	if value == nil {
		// nil is a value, not an absence. This used to return early, leaving the
		// record's field holding whatever bindRecord had bound from the client's
		// body while its present-flag stayed set — so recordSourcedWrite emitted
		// the client's value for a key the server had just cleared, and
		// SetField("owner_id", nil) stored the id the client sent (audit STEP-3).
		if !nilableKind(fv.Kind()) {
			// No nil to store in this Go type. Same remedy as a type mismatch:
			// drop the present-flag so the write falls back to ParsedBody, which
			// carries the nil. The Validate step usually refuses it first, with a
			// message naming the field.
			c.clearRecordPresent(f)
			return
		}
		fv.Set(reflect.Zero(fv.Type()))
	} else {
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
			c.clearRecordPresent(f)
			return
		}
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

// clearRecordPresent drops a column from the typed record's present-set, which
// makes the record's key set disagree with the body's and sends the write path
// down the toDBMap(ParsedBody) fallback for the whole record.
func (c *ServerContext) clearRecordPresent(f *FieldMeta) {
	if rm, ok := c.Record.(recordMeta); ok {
		if p := rm.mfxPresent(); p != nil {
			delete(p, f.Tags.DBName)
		}
	}
}

// nilableKind reports whether a Go type of this kind can hold nil, and so can
// represent a SetField(name, nil) on the typed record itself.
func nilableKind(k reflect.Kind) bool {
	switch k {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Interface,
		reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return true
	}
	return false
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
