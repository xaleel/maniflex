package maniflex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// LocaleString is a map of locale keys to translated strings.
// Stored as TEXT (SQLite) or JSONB (Postgres) in the database.
// Use with mfx:"locale" on struct fields:
//
//	type Department struct {
//	    maniflex.BaseModel
//	    Name maniflex.LocaleString `mfx:"locale"`
//	    Code string                `mfx:"required,unique"`
//	}
//
// The response representation depends on the field's LocaleMode (default: split):
//   - split: "name" is the resolved string, "name_i18n" is always the full map
//   - resolve: "name" is always the resolved string
//   - dynamic: "name" is a string when ?locale= is set, the full map otherwise
type LocaleString map[string]string

// LocaleMode controls how a LocaleString field is represented in responses.
type LocaleMode string

const (
	// LocaleModeSplit is the default: the field name holds the resolved string
	// and an auto-generated companion field (e.g. "name_i18n") always holds the
	// full locale map. Clients get a stable string type for display while still
	// having access to all translations.
	LocaleModeSplit LocaleMode = "split"

	// LocaleModeResolve always returns the field as a string. The locale is
	// determined by: ?locale= param → Accept-Language header → field
	// default_locale → model DefaultLocale → app Default → "en".
	LocaleModeResolve LocaleMode = "resolve"

	// LocaleModeDynamic replicates the legacy behavior: the field is a string
	// when ?locale= is present, the full map otherwise. Opt-in only; not
	// recommended for new models because the field type is non-deterministic.
	LocaleModeDynamic LocaleMode = "dynamic"
)

// SQLType implements SQLTyper so the DB adapter maps LocaleString to the
// correct column type: JSONB in Postgres (enables GIN index on locale keys),
// TEXT in SQLite.
func (LocaleString) SQLType(driver DriverType) string {
	if driver == Postgres {
		return "JSONB"
	}
	return "TEXT"
}

// MarshalJSON implements json.Marshaler so LocaleString round-trips cleanly.
func (ls LocaleString) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string(ls))
}

// UnmarshalJSON implements json.Unmarshaler.
func (ls *LocaleString) UnmarshalJSON(data []byte) error {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*ls = LocaleString(m)
	return nil
}

// ── LocaleOptions ──────────────────────────────────────────────────────────────

// LocaleOptions configures the LocaleResolver middleware.
type LocaleOptions struct {
	// Supported is the list of locale codes the application accepts.
	// Requests with a locale outside this list fall through to Default.
	// Empty means all locales are accepted as-is.
	Supported []string

	// Default is the locale used when the request carries no recognisable
	// locale preference. Defaults to "en" when empty.
	Default string

	// FromHeader enables Accept-Language header parsing. When true the
	// resolver picks the first value in Supported that matches a language
	// tag in the header (ignoring quality values for simplicity).
	FromHeader bool

	// RTL is the list of locale codes that use right-to-left script.
	// When the resolved locale is in this list the response meta gets
	// "_dir": "rtl".
	RTL []string

	// DefaultLocaleMode sets the app-wide default mode for LocaleString fields
	// that don't have an explicit mode in their struct tag and whose model
	// has no ModelConfig.DefaultLocaleMode. When empty, split is used.
	DefaultLocaleMode LocaleMode

	// SplitSuffix is the suffix appended to a field name for the i18n companion
	// in split mode. Defaults to "_i18n" when empty.
	SplitSuffix string
}

// LocaleResolver returns a Deserialize-step middleware that determines the
// active locale for the request and stores it on ctx.Locale.
//
// Resolution order:
//  1. ?locale= query parameter
//  2. Accept-Language header (first match in opts.Supported), when opts.FromHeader is true
//  3. opts.Default (default: "en")
//
// Usage:
//
//	server.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
//	    Supported:  []string{"en", "ar"},
//	    Default:    "en",
//	    FromHeader: true,
//	    RTL:        []string{"ar", "he", "fa", "ur"},
//	}))
func LocaleResolver(opts LocaleOptions) MiddlewareFunc {
	defaultLocale := strings.ToLower(opts.Default)
	if defaultLocale == "" {
		defaultLocale = "en"
	}

	splitSuffix := opts.SplitSuffix
	if splitSuffix == "" {
		splitSuffix = "_i18n"
	}

	supported := make(map[string]bool, len(opts.Supported))
	for _, l := range opts.Supported {
		supported[strings.ToLower(l)] = true
	}
	isSupported := func(locale string) bool {
		if len(supported) == 0 {
			return locale != ""
		}
		return supported[strings.ToLower(locale)]
	}

	rtl := make(map[string]bool, len(opts.RTL))
	for _, l := range opts.RTL {
		rtl[strings.ToLower(l)] = true
	}

	return func(ctx *ServerContext, next func() error) error {
		locale := ""

		// 1. ?locale= query parameter
		if q := ctx.QueryParam("locale"); q != "" {
			// The locale becomes a JSON-path key in filter/ORDER BY SQL, so it
			// must be a well-formed identifier (SEC-2). When Supported is empty
			// isSupported accepts any non-empty string, so isLocaleKey is what
			// rejects a crafted value; an invalid value is ignored and falls
			// back to the default, exactly like an unsupported one.
			if n := strings.ToLower(q); isSupported(n) && isLocaleKey(n) {
				locale = n
			}
		}

		// 2. Accept-Language header
		if locale == "" && opts.FromHeader {
			locale = parseAcceptLanguage(ctx.Request, supported)
		}

		// ctx.Locale is only set when the client explicitly requested a locale.
		// When neither param nor header is present ctx.Locale stays "" and
		// LocaleString fields fall back to mode-specific defaults.
		ctx.Locale = locale

		// Store app-level defaults so toJSONMap can use them.
		ctx.DefaultLocale = defaultLocale
		ctx.SplitSuffix = splitSuffix
		ctx.DefaultLocaleMode = opts.DefaultLocaleMode

		effective := locale
		if effective == "" {
			effective = defaultLocale
		}
		if rtl[effective] {
			ctx.Set("_rtl", true)
		}

		return next()
	}
}

// parseAcceptLanguage returns the first locale from the Accept-Language header
// that is present in the supported set. Returns "" when no match is found.
func parseAcceptLanguage(r *http.Request, supported map[string]bool) string {
	header := r.Header.Get("Accept-Language")
	if header == "" {
		return ""
	}
	// Each entry is like "ar-SA;q=0.9" or "en" — split by comma, strip quality.
	for _, part := range strings.Split(header, ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if tag == "" {
			continue
		}
		// Exact match first.
		if supported[tag] {
			return tag
		}
		// Try the base language (e.g. "ar-SA" → "ar").
		if base := strings.SplitN(tag, "-", 2)[0]; supported[base] {
			return base
		}
	}
	return ""
}

// ── Locale resolution helpers ─────────────────────────────────────────────────

// effectiveLocaleMode resolves the LocaleMode for a field following the
// precedence: field tag → model config → app LocaleOptions → split (default).
func effectiveLocaleMode(field *FieldMeta, model *ModelMeta, ctx *ServerContext) LocaleMode {
	if field.Tags.LocaleMode != "" {
		return field.Tags.LocaleMode
	}
	if model != nil && model.Config.DefaultLocaleMode != "" {
		return model.Config.DefaultLocaleMode
	}
	if ctx != nil && ctx.DefaultLocaleMode != "" {
		return ctx.DefaultLocaleMode
	}
	return LocaleModeSplit
}

// effectiveLocaleChain builds the ordered list of locale keys to try when
// resolving a LocaleString field, from most to least preferred.
// The chain always ends with "en" so there is always a last-resort fallback.
func effectiveLocaleChain(ctx *ServerContext, field *FieldMeta, model *ModelMeta) []string {
	seen := make(map[string]bool)
	var chain []string
	add := func(l string) {
		if l != "" && !seen[l] {
			seen[l] = true
			chain = append(chain, l)
		}
	}

	if ctx != nil {
		add(ctx.Locale) // explicit request locale
	}
	if field != nil {
		add(field.Tags.LocaleDefault) // field-level default
	}
	if model != nil {
		add(model.Config.DefaultLocale) // model-level default
	}
	if ctx != nil {
		add(ctx.DefaultLocale) // app-level default from LocaleOptions
	}
	add("en") // hard fallback
	return chain
}

// resolveLocaleString returns the best string from a map[string]string given
// an ordered chain of locale keys (most to least preferred).
// Falls back to the first non-empty value in the map when none of the chain
// keys match.
func resolveLocaleString(m map[string]string, chain []string) string {
	for _, locale := range chain {
		if v, ok := m[locale]; ok && v != "" {
			return v
		}
	}
	// Last resort: the non-empty value under the lexicographically smallest key.
	//
	// This used to range the map and return the first non-empty value it saw,
	// which Go deliberately randomises: a field with two or more locales outside
	// the chain rendered differently on every read. That is not merely untidy —
	// the response feeds the ETag, so the same unchanged row produced a new ETag
	// each time and optimistic-lock writes failed with spurious 412s, and the
	// response cache could never hit (audit MS-L5). Any fixed rule would do; the
	// smallest key is the one a reader can predict.
	best := ""
	for k, v := range m {
		if v == "" {
			continue
		}
		if best == "" || k < best {
			best = k
		}
	}
	if best == "" {
		return ""
	}
	return m[best]
}

// localeStringToMap parses a DB-stored locale value into a map[string]string.
// Handles string (JSON TEXT), []byte, and map[string]any (Postgres JSONB).
// Returns nil when the value cannot be parsed.
func localeStringToMap(v any) map[string]string {
	m, _ := localeValueToMap(v)
	return m
}

// localeValueToMap is localeStringToMap plus whether the value had to be folded
// from a bare JSON scalar. Callers with a logger report that: the row is
// readable but the column is corrupt, and silently normalising it would hide
// data that still needs fixing.
func localeValueToMap(v any) (m map[string]string, folded bool) {
	if v == nil {
		return nil, false
	}
	switch s := v.(type) {
	case LocaleString:
		// A scanStruct-populated locale field arrives as the named type.
		return map[string]string(s), false
	case map[string]string:
		return s, false
	case map[string]any:
		out := make(map[string]string, len(s))
		for k, val := range s {
			if str, ok := val.(string); ok {
				out[k] = str
			}
		}
		return out, false
	case string:
		return localeTextToMap([]byte(s))
	case []byte:
		return localeTextToMap(s)
	}
	return nil, false
}

// localeTextToMap parses stored locale JSON, falling back to FoldLocaleScalar
// when the column holds a bare JSON scalar rather than an object.
func localeTextToMap(raw []byte) (map[string]string, bool) {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err == nil {
		return m, false
	}
	if folded, ok := FoldLocaleScalar(raw); ok {
		return folded, true
	}
	return nil, false
}

// FoldLocaleScalar reads a locale column that holds a bare JSON scalar —
// "Cardiology" rather than {"en":"Cardiology"} — and returns it as a one-key
// map so the value stays readable.
//
// Such a column is corrupt: nothing writes this shape any more (see
// localeWriteValue), but rows written before v0.2.5 carry it, and a hard scan
// error made them unrecoverable through the API — a 500 on the record and, since
// one bad row fails the list scan, on the whole collection endpoint. Degrading
// keeps the endpoint up and the row editable, which is what lets an operator fix
// it by PATCHing a proper map.
//
// The key is "en", matching effectiveLocaleChain's own hard fallback. Which key
// it is barely matters for display: resolveLocaleString falls back to any
// non-empty value when no chain key matches, so the value surfaces under every
// requested locale either way.
//
// Reported by the caller as a warning, not swallowed — see the "locale field
// held a bare scalar" log in toJSONMap/marshalRecord.
func FoldLocaleScalar(raw []byte) (map[string]string, bool) {
	var scalar any
	if err := json.Unmarshal(raw, &scalar); err != nil {
		return nil, false
	}
	switch x := scalar.(type) {
	case string:
		return map[string]string{"en": x}, true
	case float64, bool:
		return map[string]string{"en": fmt.Sprint(x)}, true
	}
	return nil, false
}
