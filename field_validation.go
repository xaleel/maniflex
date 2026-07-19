package maniflex

// field_validation.go — the model's own value constraints (enum, min, max,
// required), factored out of the Validate step so the programmatic write paths
// enforce them too.
//
// Before v0.2.5 these checks lived inline in defaultSteps.validate, operating on
// ctx.ParsedBody, and so ran for HTTP requests and nothing else. maniflex.Create
// / Update and ctx.GetModel(...).Create / Update went straight to the adapter,
// which meant the same model accepted a value in Go that it answered 422 for
// over HTTP — an out-of-enum role, a balance past its max — and stored it
// (audit MS-15).
//
// What is *not* shared with the Validate step: readonly and immutable stripping.
// Those tags say a value may not come from a client, and the typed helpers are
// not a client — a background job stamping a readonly column is the reason those
// helpers exist. The distinction is between data integrity, which is a bug
// whoever writes it, and access control, which depends on who is asking.
// (Update[T] does exclude readonly/immutable, but for an unrelated reason: a
// full-struct write would otherwise stamp zero values over them — audit MS-5.)

import (
	"fmt"
	"reflect"
)

// WriteOption adjusts a programmatic write. See SkipValidation.
type WriteOption func(*writeOptions)

type writeOptions struct {
	skipValidation bool
}

// SkipValidation turns off the model's value constraints for one write:
//
//	_, err := maniflex.Create(ctx, &acct, maniflex.SkipValidation())
//
// Use it where a violation is deliberate — backfilling rows that predate an
// enum, or importing legacy data an app intends to clean up later. It is a
// per-call argument rather than a mode so the exception is visible at the site
// that takes it, not somewhere up the stack.
func SkipValidation() WriteOption {
	return func(o *writeOptions) { o.skipValidation = true }
}

func applyWriteOptions(opts []WriteOption) writeOptions {
	var o writeOptions
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// validateRecordValues checks a *T's fields against the model's value
// constraints. present limits the check to the columns the write will actually
// send, so a field omitted from the INSERT is not judged on the zero value that
// is never stored. A nil present set checks every field.
func validateRecordValues(meta *ModelMeta, record any, present map[string]struct{}) error {
	rv := reflect.ValueOf(record)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil
	}
	rv = rv.Elem()
	var errs []string
	for i := range meta.Fields {
		f := &meta.Fields[i]
		if present != nil {
			if _, ok := present[f.Tags.DBName]; !ok {
				continue
			}
		}
		fv := rv.FieldByIndex(f.Index)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				if f.Tags.Required {
					errs = append(errs, fmt.Sprintf("field %q is required", f.Tags.JSONName))
				}
				continue
			}
			fv = fv.Elem()
		}
		if msg := checkFieldValue(f, fv.Interface()); msg != "" {
			errs = append(errs, msg)
		}
	}
	return validationError(meta, errs)
}

// validateDataValues is validateRecordValues for the accessor's DB-column-keyed
// maps. Only keys the map actually carries are checked, matching the partial
// nature of an accessor write.
func validateDataValues(meta *ModelMeta, data map[string]any) error {
	var errs []string
	for i := range meta.Fields {
		f := &meta.Fields[i]
		v, ok := data[f.Tags.DBName]
		if !ok || v == nil {
			continue
		}
		if msg := checkFieldValue(f, v); msg != "" {
			errs = append(errs, msg)
		}
	}
	return validationError(meta, errs)
}

// checkFieldValue applies one field's enum and min/max constraints, returning a
// message or "". It mirrors the Validate step's wording so the two paths report
// a violation the same way.
func checkFieldValue(f *FieldMeta, val any) string {
	jn := f.Tags.JSONName
	if len(f.Tags.Enum) > 0 {
		str := fmt.Sprintf("%v", val)
		found := false
		for _, e := range f.Tags.Enum {
			if e == str {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("field %q must be one of: %v", jn, f.Tags.Enum)
		}
	}
	if f.Tags.Min != nil || f.Tags.Max != nil {
		num, ok := toFloat64(val)
		if !ok {
			// A value the bound cannot be applied to used to skip the check
			// entirely, so a numeric field sent "abc" passed validation and was
			// left to the database to reject, or silently coerced (audit
			// MS-L11). A bound the value cannot be measured against is a failed
			// bound, not an absent one.
			return fmt.Sprintf("field %q must be a number", jn)
		}
		if f.Tags.Min != nil && num < *f.Tags.Min {
			return fmt.Sprintf("field %q must be >= %g", jn, *f.Tags.Min)
		}
		if f.Tags.Max != nil && num > *f.Tags.Max {
			return fmt.Sprintf("field %q must be <= %g", jn, *f.Tags.Max)
		}
	}
	return ""
}

func validationError(meta *ModelMeta, errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("maniflex: %s: %s (pass maniflex.SkipValidation() if deliberate)",
		meta.Name, joinMessages(errs))
}

func joinMessages(errs []string) string {
	out := errs[0]
	for _, e := range errs[1:] {
		out += "; " + e
	}
	return out
}
