package maniflex

// body_typing.go — refusing a write body whose values the model's Go types
// cannot hold (audit STEP-6).
//
// Deserialize decodes the body into the typed record best-effort: a type
// mismatch leaves ctx.Record nil and the DB step writes ParsedBody verbatim.
// SQLite stores whatever it is given — 'abc' goes into an INTEGER column
// without complaint — and the typed scan on the way back out then fails the
// whole result set, so one such row answered 500 on every list and every read
// of that collection, for every caller. Postgres refuses the INSERT instead and
// surfaces it as an opaque 500. Neither is an answer the client can act on.
//
// The two doors into that state need different treatment, because they carry
// different things:
//
//   - A JSON body already has types. Whether a value fits is exactly what
//     encoding/json answers when it decodes into the field, so the check here
//     decodes each field's raw bytes into its own Go type and reports the ones
//     that fail. Using json itself as the oracle is the point: the rule is the
//     one the typed record path already applies, with no second rule set to
//     drift from it. It covers the fields whose column holds exactly one shape
//     — see jsonDecides for the types that own their own and are left alone.
//
//   - A multipart form carries only strings, so the same test would reject every
//     numeric field a form has ever sent. Those are converted to the field's Go
//     type instead, and only a string that will not convert is refused.

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var (
	timeType            = reflect.TypeOf(time.Time{})
	jsonUnmarshalerType = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	sqlScannerType      = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)

// bodyTypeError reports why the JSON body's value for this field cannot be held
// by the field's Go type, or "" when it can (which includes every field of a
// body that decoded cleanly).
//
// It runs only when the whole-body decode failed, so an ordinary request pays
// nothing: if the body decoded into *T, no field in it is mistyped. When one is,
// json stops recording after the first, so this pass re-decodes field by field
// to name all of them — the shape the Validate step reports every other rule in.
func bodyTypeError(ctx *ServerContext, f *FieldMeta) string {
	if ctx == nil || !ctx.bodyDecodeFailed || ctx.bodyRaw == nil || f.Type == nil {
		return ""
	}
	// Locale columns accept both shapes split mode emits — the full map and the
	// bare string a read answered with — and LocaleString.UnmarshalJSON accepts
	// only the map. The bare string failing the decode is what routes the write
	// through localeWriteValue, which understands both, so refusing it here would
	// break the documented round-trip rather than catch anything.
	if f.Tags.Locale {
		return ""
	}
	if !jsonDecides(f.Type) {
		return scannerTypeError(ctx, f)
	}
	raw, ok := ctx.bodyRaw[f.Tags.JSONName]
	if !ok {
		return ""
	}
	if err := json.Unmarshal(raw, reflect.New(f.Type).Interface()); err != nil {
		return typeErrorMessage(f.Type)
	}
	return ""
}

// jsonDecides reports whether a failed decode into this type means the body is
// wrong, rather than that the body is in the type's other accepted shape.
//
// A type carrying its own UnmarshalJSON has declared one shape, and for the ones
// the framework ships that is not the only shape a client may send: money.Amount
// decodes {"amount", "currency"} but a bare 12.34 is written through the map
// path and read back by its Scan, and LocaleString decodes a map while split mode
// answers reads with a bare string. Refusing the second shape would break writes
// that work today, and the type — not this file — is the authority on what it
// accepts. So the check covers the plain scalars, whose column can hold exactly
// one shape, plus time.Time, whose alternative shape is a string the database
// cannot read back.
func jsonDecides(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return true
	}
	if reflect.PointerTo(t).Implements(jsonUnmarshalerType) {
		return false
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// scannerTypeMessage is what a type that owns its own SQL representation gets:
// the framework cannot enumerate the forms it accepts, only report that this
// one is not among them.
const scannerTypeMessage = "is not a value this field can hold"

// scannerTypeError judges a value against the two doors a column typed by an
// SQLTyper actually has, since json alone answers only one of them.
//
// money.Amount is the shipped example: it decodes {"amount", "currency"} through
// UnmarshalJSON, and a bare 12.34 reaches the column through the map path and is
// read back by Scan. Both work and both must keep working, so the test is
// whether either accepts the value — and "abc", which neither does, is refused
// here instead of being stored and failing the collection's next scan.
//
// Scan is offered the value the map path would write. That is not always byte-
// identical to what the driver hands back on the way out (12.34 goes in as a
// float and returns as a decimal string), so a type whose Scan is narrower than
// its own round-trip would refuse a write that works today. Types that accept
// what they emit — the contract Scan/Value already implies — are unaffected.
func scannerTypeError(ctx *ServerContext, f *FieldMeta) string {
	t := f.Type
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if !reflect.PointerTo(t).Implements(sqlScannerType) {
		return ""
	}
	raw, ok := ctx.bodyRaw[f.Tags.JSONName]
	if !ok {
		return ""
	}
	if err := json.Unmarshal(raw, reflect.New(t).Interface()); err == nil {
		return ""
	}
	v, ok := ctx.ParsedBody.Get(f.Tags.JSONName)
	if !ok || v == nil {
		return ""
	}
	if sc, ok := reflect.New(t).Interface().(sql.Scanner); ok {
		if err := sc.Scan(v); err != nil {
			return scannerTypeMessage
		}
	}
	return ""
}

// typeErrorMessage names the shape a field will accept, in the client's terms.
//
// json's own error text would name Go types ("cannot unmarshal string into Go
// struct field Widget.age of type int"), which tells a client nothing it can act
// on and would become a frozen wire string at v1.0.
func typeErrorMessage(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return "must be an RFC 3339 timestamp"
	}
	switch t.Kind() {
	case reflect.String:
		return "must be a string"
	case reflect.Bool:
		return "must be true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "must be a whole number"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "must be a whole number, zero or greater"
	case reflect.Float32, reflect.Float64:
		return "must be a number"
	case reflect.Slice, reflect.Array:
		return "must be an array"
	case reflect.Map, reflect.Struct:
		return "must be an object"
	}
	return "is not a value this field can hold"
}

// coerceFormValues converts each multipart form value into the Go type its
// column holds, and records the ones that will not convert.
//
// Failures are recorded rather than aborted on: Validate strips readonly and
// immutable fields before it reads anything else, so a value that was never
// going to be written must not produce an error about its type. Recording here
// and reporting there keeps that order intact, and puts a form's type errors in
// the same 422 as its missing required fields rather than in one of its own.
func coerceFormValues(ctx *ServerContext) {
	if ctx == nil || ctx.Model == nil || ctx.ParsedBody == nil {
		return
	}
	for i := range ctx.Model.Fields {
		f := &ctx.Model.Fields[i]
		if f.Type == nil || f.Tags.Locale {
			continue
		}
		str, ok := ctx.ParsedBody.m[f.Tags.JSONName].(string)
		if !ok {
			continue
		}
		v, msg, handled := coerceFormValue(f.Type, str)
		switch {
		case !handled:
			// Left exactly as it arrived, as it always has been: a string column
			// wants the string, and a type with its own SQL representation knows
			// how to read one better than this does.
		case msg != "":
			if ctx.formTypeErrs == nil {
				ctx.formTypeErrs = make(map[string]string, 1)
			}
			ctx.formTypeErrs[f.Tags.JSONName] = msg
		default:
			ctx.ParsedBody.m[f.Tags.JSONName] = v
		}
	}
}

// coerceFormValue converts one form value to the Go type the column holds.
//
// handled is false for the kinds a form value is already the right shape for,
// which are passed through untouched. Otherwise value is what to write and msg
// is empty, or msg says why the string will not do and value is meaningless.
func coerceFormValue(t reflect.Type, s string) (value any, msg string, handled bool) {
	et := t
	for et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	kind := et.Kind()
	isTime := et == timeType
	isNumeric := (kind >= reflect.Int && kind <= reflect.Uint64) ||
		kind == reflect.Float32 || kind == reflect.Float64
	// A type that owns its SQL representation is asked, below, whether it can
	// read the string — the same question the column's next read will put to it.
	isScanner := reflect.PointerTo(et).Implements(sqlScannerType)
	switch {
	case isTime, isScanner, isNumeric:
	case kind == reflect.Bool:
	default:
		return nil, "", false
	}

	// A browser posts every input in the form, including the ones nobody filled
	// in, so an empty value is "no value" rather than a malformed one. Reading it
	// as null hands it to the rule that already decides what a column does with
	// one: a pointer field stores NULL, and a field whose type has no null is
	// refused by name (audit MS-7) rather than given an invented zero.
	if strings.TrimSpace(s) == "" {
		return nil, "", true
	}

	if isTime {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, typeErrorMessage(t), true
		}
		return v, "", true
	}

	// Checked after the kinds above, so a named bool or numeric type that also
	// carries a Scan is still converted rather than left as text.
	if isScanner && !isNumeric && kind != reflect.Bool {
		if sc, ok := reflect.New(et).Interface().(sql.Scanner); ok {
			if err := sc.Scan(s); err != nil {
				return nil, scannerTypeMessage, true
			}
		}
		return s, "", true
	}

	switch {
	case kind == reflect.Bool:
		if b, ok := parseBoolWord(s); ok {
			return b, "", true
		}
		// A checked checkbox posts "on" and an unchecked one posts nothing at
		// all, so the pair is a form's spelling of the boolean rather than a
		// typo. parseBoolWord stays as it is: it answers ?filter= too, where
		// these spellings have never been part of the contract.
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "on":
			return true, "", true
		case "off":
			return false, "", true
		}
		return nil, typeErrorMessage(t), true

	case kind >= reflect.Int && kind <= reflect.Int64:
		n, err := strconv.ParseInt(s, 10, bitSizeOf(kind))
		if err != nil {
			return nil, typeErrorMessage(t), true
		}
		return n, "", true

	case kind >= reflect.Uint && kind <= reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, bitSizeOf(kind))
		if err != nil {
			return nil, typeErrorMessage(t), true
		}
		return n, "", true

	default:
		bits := 64
		if kind == reflect.Float32 {
			bits = 32
		}
		n, err := strconv.ParseFloat(s, bits)
		if err != nil {
			return nil, typeErrorMessage(t), true
		}
		return n, "", true
	}
}

// bitSizeOf gives strconv the width the column actually holds, so a value too
// large for an int8 is refused here rather than silently truncated on the way
// into the database.
func bitSizeOf(k reflect.Kind) int {
	switch k {
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32:
		return 32
	}
	return 64
}
