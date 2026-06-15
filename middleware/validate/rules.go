// Package validate provides Validate-step middleware for advanced field rules
// that cannot be expressed with static mfx struct tags.
package validate

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"maniflex"
	"maniflex/db/sqlcore"
)

// ── UniqueField ───────────────────────────────────────────────────────────────

// DBQuerier is the minimal interface UniqueField needs to check uniqueness.
// *sql.DB satisfies this interface; so does any wrapper that forwards
// QueryRowContext (e.g. a test fake).
type DBQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// UniqueField runs a SELECT COUNT(*) before the DB step to verify that the
// given field's value does not already exist in the table. Returns 422 if a
// duplicate is found, with a user-friendly error message.
//
// `field` is the JSON field name as it appears in ctx.ParsedBody; the actual
// DB column is resolved through ctx.Model.FieldByJSONName. If the JSON name
// has no matching field on the model, `field` is used verbatim as a DB
// column name (preserves backward-compat for callers that already used DB
// names).
//
// `driver` selects the SQL dialect for placeholders (`$N` for Postgres, `?`
// for SQLite) and is required because *sql.DB does not expose its driver.
// Pass the same driver value that was used to open the maniflex DB adapter.
//
// On update operations, the current record's own row is excluded from the
// check (so saving without changing the unique field does not produce a
// false positive).
//
//	server.Pipeline.Validate.Register(
//	    validate.UniqueField(db, maniflex.Postgres, "email"),
//	    maniflex.ForModel("User"),
//	)
func UniqueField(db DBQuerier, driver maniflex.DriverType, field string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		val, ok := ctx.ParsedBody.Get(field)
		if !ok || val == nil {
			return next()
		}

		// Resolve JSON field name → DB column name. Fall back to the raw
		// argument so callers that historically passed a DB name still work.
		dbCol := field
		if ctx.Model != nil {
			if f := ctx.Model.FieldByJSONName(field); f != nil {
				dbCol = f.Tags.DBName
			}
		}

		pb := sqlcore.NewPlaceholderBuilder(driver)
		valPH := pb.Add(val)

		var query string
		if ctx.Operation == maniflex.OpUpdate && ctx.ResourceID != "" {
			idPH := pb.Add(ctx.ResourceID)
			query = fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = %s AND %s != %s",
				sqlcore.Quote(ctx.Model.TableName),
				sqlcore.Quote(dbCol), valPH,
				sqlcore.Quote("id"), idPH,
			)
		} else {
			query = fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = %s",
				sqlcore.Quote(ctx.Model.TableName),
				sqlcore.Quote(dbCol), valPH,
			)
		}

		var count int
		if err := db.QueryRowContext(ctx.Ctx, query, pb.Args()...).Scan(&count); err != nil {
			// Surface the underlying error — previously it was silently
			// swallowed, which meant a malformed query (e.g. wrong placeholder
			// dialect) passed validation and a duplicate write could succeed.
			ctx.Abort(http.StatusInternalServerError, "UNIQUE_CHECK_FAILED",
				fmt.Sprintf("unique check failed for field %q: %s", field, err.Error()))
			return nil
		}
		if count > 0 {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: "validation failed",
					Details: []map[string]string{{
						"field":   field,
						"message": fmt.Sprintf("%s is already taken", field),
					}},
				},
			}
			return nil
		}
		return next()
	}
}

// ── NumericPrecision ──────────────────────────────────────────────────────────

// NumericPrecision enforces decimal precision and scale on an incoming numeric
// field. `precision` is the maximum total number of significant digits;
// `scale` is the maximum number of digits after the decimal point. Either
// limit can be disabled by passing 0.
//
// The check is a string-parse of the JSON value, so it is independent of how
// the column is stored (INTEGER, NUMERIC(p,s), TEXT, DECIMAL). Numbers are
// normalised by trimming a leading "+"/"-", stripping the decimal point, and
// removing leading zeros from the integer part before counting digits.
//
// Non-numeric strings, absent fields, and nil values are skipped — pair with
// the `required` mfx tag or another rule when presence matters.
//
//	server.Pipeline.Validate.Register(
//	    validate.NumericPrecision("amount", 19, 4), // up to 19 digits, max 4 after the point
//	    maniflex.ForModel("Invoice"),
//	)
func NumericPrecision(field string, precision, scale int) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		val, ok := ctx.ParsedBody.Get(field)
		if !ok || val == nil {
			return next()
		}
		raw, ok := numericString(val)
		if !ok {
			return next()
		}
		intPart, fracPart, valid := splitDecimal(raw)
		if !valid {
			ctx.Response = validationDetail(field, fmt.Sprintf("field %q is not a valid number", field))
			return nil
		}
		if scale > 0 && len(fracPart) > scale {
			ctx.Response = validationDetail(field,
				fmt.Sprintf("field %q must have at most %d decimal place(s)", field, scale))
			return nil
		}
		if precision > 0 {
			sig := len(intPart) + len(fracPart)
			if sig > precision {
				ctx.Response = validationDetail(field,
					fmt.Sprintf("field %q must have at most %d total digit(s)", field, precision))
				return nil
			}
		}
		return next()
	}
}

// numericString returns the canonical numeric representation of v as it would
// appear in JSON. Returns (value, true) for json.Number, float64, integer
// types, and numeric-looking strings; (_, false) for everything else.
func numericString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return "", false
		}
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return "", false
		}
		return s, true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.FormatInt(int64(x), 10), true
	case int8:
		return strconv.FormatInt(int64(x), 10), true
	case int16:
		return strconv.FormatInt(int64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint8:
		return strconv.FormatUint(uint64(x), 10), true
	case uint16:
		return strconv.FormatUint(uint64(x), 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	}
	return "", false
}

// splitDecimal returns the integer and fractional digit strings of a numeric
// literal (with sign and leading zeros stripped). It refuses scientific
// notation — financial values must be supplied in plain form.
func splitDecimal(s string) (intPart, fracPart string, ok bool) {
	if s == "" {
		return "", "", false
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	if strings.ContainsAny(s, "eE") {
		return "", "", false
	}
	parts := strings.SplitN(s, ".", 2)
	intPart = strings.TrimLeft(parts[0], "0")
	if len(parts) == 2 {
		fracPart = strings.TrimRight(parts[1], "0")
	}
	// "0", "0.0" → both parts empty after trim; treat as a single significant
	// digit for sane behavior at the boundary.
	if intPart == "" && fracPart == "" {
		intPart = "0"
	}
	return intPart, fracPart, true
}

func validationDetail(field, message string) *maniflex.APIResponse {
	return &maniflex.APIResponse{
		StatusCode: http.StatusUnprocessableEntity,
		Error: &maniflex.APIError{
			Code:    "VALIDATION_ERROR",
			Message: "validation failed",
			Details: []map[string]string{{
				"field":   field,
				"message": message,
			}},
		},
	}
}

// ── CrossFieldValidate ─────────────────────────────────────────────────────────

// CrossFieldValidateFunc is a user-supplied multi-field validation function.
// Return a non-nil error to fail validation; the error message is included in
// the 422 response.
type CrossFieldValidateFunc func(body map[string]any) error

// CrossFieldValidate runs an arbitrary function against ctx.ParsedBody and
// returns 422 if it returns an error. Use this for rules that involve multiple
// fields (e.g. end_date must be after start_date).
//
//	server.Pipeline.Validate.Register(validate.CrossFieldValidate(func(body map[string]any) error {
//	    start, _ := body["start_date"].(string)
//	    end, _   := body["end_date"].(string)
//	    if start != "" && end != "" && end <= start {
//	        return fmt.Errorf("end_date must be after start_date")
//	    }
//	    return nil
//	}), maniflex.ForModel("Event"))
func CrossFieldValidate(fn CrossFieldValidateFunc) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		if err := fn(ctx.ParsedBody.Map()); err != nil {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: err.Error(),
				},
			}
			return nil
		}
		return next()
	}
}

// ── RegexField ────────────────────────────────────────────────────────────────

// RegexField validates that a field's string value matches the given regular
// expression. Non-string values and absent fields are silently skipped.
// Returns 422 if the field is present but does not match.
//
//	server.Pipeline.Validate.Register(
//	    validate.RegexField("phone", `^\+?[0-9\s\-]{7,15}$`),
//	    maniflex.ForModel("Contact"),
//	)
func RegexField(field, pattern string) maniflex.MiddlewareFunc {
	re := regexp.MustCompile(pattern) // panics at startup if pattern is invalid
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		val, ok := ctx.ParsedBody.Get(field)
		if !ok || val == nil {
			return next()
		}
		str, ok := val.(string)
		if !ok {
			return next()
		}
		if !re.MatchString(str) {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: "validation failed",
					Details: []map[string]string{{
						"field":   field,
						"message": fmt.Sprintf("field %q has an invalid format", field),
					}},
				},
			}
			return nil
		}
		return next()
	}
}

// ── ForbiddenValues ───────────────────────────────────────────────────────────

// ForbiddenValues rejects the request with 422 if the named field contains any
// of the given values. Useful for preventing privilege escalation (e.g. a user
// setting their own role to "superadmin").
//
//	server.Pipeline.Validate.Register(
//	    validate.ForbiddenValues("role", "superadmin", "system"),
//	    maniflex.ForModel("User"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
func ForbiddenValues(field string, values ...string) maniflex.MiddlewareFunc {
	forbidden := make(map[string]bool, len(values))
	for _, v := range values {
		forbidden[v] = true
	}
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		val, ok := ctx.ParsedBody.Get(field)
		if !ok || val == nil {
			return next()
		}
		str := fmt.Sprintf("%v", val)
		if forbidden[str] {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: "validation failed",
					Details: []map[string]string{{
						"field":   field,
						"message": fmt.Sprintf("value %q is not permitted for field %q", str, field),
					}},
				},
			}
			return nil
		}
		return next()
	}
}

// ── RequireLocale ─────────────────────────────────────────────────────────────

// RequireLocale ensures that a maniflex.LocaleString field (mfx:"locale") in the
// request body contains non-empty values for every required locale key. Returns
// 422 MISSING_LOCALE with the list of missing keys when any are absent.
//
//	server.Pipeline.Validate.Register(
//	    validate.RequireLocale("name", "en", "ar"),
//	    maniflex.ForModel("Department"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
func RequireLocale(field string, locales ...string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		val, ok := ctx.ParsedBody.Get(field)
		if !ok || val == nil {
			return next()
		}

		// Accept map[string]any (JSON-decoded) or map[string]string (Go-native)
		var localeMap map[string]any
		switch m := val.(type) {
		case map[string]any:
			localeMap = m
		case map[string]string:
			localeMap = make(map[string]any, len(m))
			for k, v := range m {
				localeMap[k] = v
			}
		default:
			// Not a map — not a LocaleString; skip silently
			return next()
		}

		var missing []string
		for _, locale := range locales {
			v, exists := localeMap[locale]
			if !exists || v == nil || fmt.Sprintf("%v", v) == "" {
				missing = append(missing, locale)
			}
		}
		if len(missing) > 0 {
			details := make([]map[string]string, len(missing))
			for i, loc := range missing {
				details[i] = map[string]string{
					"field":   field,
					"message": fmt.Sprintf("locale key %q is required for field %q", loc, field),
				}
			}
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "MISSING_LOCALE",
					Message: fmt.Sprintf("field %q is missing required locale(s): %s", field, strings.Join(missing, ", ")),
					Details: details,
				},
			}
			return nil
		}
		return next()
	}
}

// ── DateRange ─────────────────────────────────────────────────────────────────

// DateRange validates that endField is not before startField. Both fields must
// be RFC3339 timestamps or YYYY-MM-DD date strings. If either field is absent,
// nil, or unparseable, the rule passes silently — pair with the `required` mfx
// tag or another rule when presence matters.
//
//	server.Pipeline.Validate.Register(
//	    validate.DateRange("start_date", "end_date"),
//	    maniflex.ForModel("Booking"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
func DateRange(startField, endField string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		body := ctx.ParsedBody.Map()
		start, ok := parseTimeField(body, startField)
		if !ok {
			return next()
		}
		end, ok := parseTimeField(body, endField)
		if !ok {
			return next()
		}
		if end.Before(start) {
			ctx.Response = validationDetail(endField,
				fmt.Sprintf("field %q must not be before %q", endField, startField))
			return nil
		}
		return next()
	}
}

// parseTimeField extracts and parses a time value from body[field].
// Accepts RFC3339 timestamps and YYYY-MM-DD date strings.
func parseTimeField(body map[string]any, field string) (time.Time, bool) {
	val, ok := body[field]
	if !ok || val == nil {
		return time.Time{}, false
	}
	s, ok := val.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// ── RequireWhen ───────────────────────────────────────────────────────────────

// RequireWhen makes targetField required when all listed conditions are satisfied.
// Each condition has the form "field:op:value" where op is one of:
//
//	eq  — string equality
//	ne  — string inequality
//	gt  — numeric greater-than
//	gte — numeric greater-than-or-equal
//	lt  — numeric less-than
//	lte — numeric less-than-or-equal
//
// Multiple conditions are ANDed: all must hold for the requirement to trigger.
// If the conditions are met but targetField is absent, nil, or empty string,
// the request is rejected with 422 VALIDATION_ERROR.
// Invalid condition syntax panics at registration time (startup), not at
// request time.
//
//	server.Pipeline.Validate.Register(
//	    validate.RequireWhen("rejection_reason", "status:eq:rejected"),
//	    maniflex.ForModel("Claim"),
//	)
//
//	server.Pipeline.Validate.Register(
//	    validate.RequireWhen("shipping_address", "order_type:eq:physical", "region:ne:digital"),
//	    maniflex.ForModel("Order"),
//	)
func RequireWhen(targetField string, conditions ...string) maniflex.MiddlewareFunc {
	type parsedCond struct{ field, op, value string }
	conds := make([]parsedCond, 0, len(conditions))
	for _, c := range conditions {
		parts := strings.SplitN(c, ":", 3)
		if len(parts) != 3 {
			panic(fmt.Sprintf("validate.RequireWhen: invalid condition %q (want field:op:value)", c))
		}
		conds = append(conds, parsedCond{parts[0], parts[1], parts[2]})
	}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		body := ctx.ParsedBody.Map()
		for _, c := range conds {
			if !requireWhenCond(body, c.field, c.op, c.value) {
				return next()
			}
		}
		v, exists := body[targetField]
		if !exists || v == nil || fmt.Sprintf("%v", v) == "" {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: "validation failed",
					Details: []map[string]string{{
						"field":   targetField,
						"message": fmt.Sprintf("field %q is required", targetField),
					}},
				},
			}
			return nil
		}
		return next()
	}
}

// requireWhenCond evaluates a single RequireWhen condition against body[field].
func requireWhenCond(body map[string]any, field, op, value string) bool {
	v, ok := body[field]
	if !ok || v == nil {
		return false
	}
	str := fmt.Sprintf("%v", v)
	switch op {
	case "eq":
		return str == value
	case "ne":
		return str != value
	case "gt", "gte", "lt", "lte":
		bv, err1 := strconv.ParseFloat(str, 64)
		cv, err2 := strconv.ParseFloat(value, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		switch op {
		case "gt":
			return bv > cv
		case "gte":
			return bv >= cv
		case "lt":
			return bv < cv
		case "lte":
			return bv <= cv
		}
	}
	return false
}

// ── RequireAtLeastOne ─────────────────────────────────────────────────────────

// RequireAtLeastOne returns 422 if none of the listed fields are present and
// non-nil in ctx.ParsedBody. Useful for PATCH endpoints where the body must
// contain at least one meaningful field.
//
//	server.Pipeline.Validate.Register(
//	    validate.RequireAtLeastOne("name", "email", "phone"),
//	    maniflex.ForModel("Contact"),
//	    maniflex.ForOperation(maniflex.OpUpdate),
//	)
func RequireAtLeastOne(fields ...string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			ctx.Response = &maniflex.APIResponse{
				StatusCode: http.StatusUnprocessableEntity,
				Error: &maniflex.APIError{
					Code:    "VALIDATION_ERROR",
					Message: fmt.Sprintf("at least one of the following fields is required: %s", strings.Join(fields, ", ")),
				},
			}
			return nil
		}
		for _, f := range fields {
			if v, ok := ctx.ParsedBody.Get(f); ok && v != nil {
				return next()
			}
		}
		ctx.Response = &maniflex.APIResponse{
			StatusCode: http.StatusUnprocessableEntity,
			Error: &maniflex.APIError{
				Code:    "VALIDATION_ERROR",
				Message: fmt.Sprintf("at least one of the following fields is required: %s", strings.Join(fields, ", ")),
			},
		}
		return nil
	}
}
