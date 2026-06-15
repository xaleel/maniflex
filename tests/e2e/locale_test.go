package e2e

// Phase 6 — Localisation (Modes: split, resolve, dynamic)
//
// Split mode (default): field = resolved string, field_i18n = full map
// Resolve mode:         field = always a string (uses locale chain)
// Dynamic mode:         field = string when ?locale= set, full map otherwise
//
// Tests are grouped by mode, then by feature within each mode.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/validate"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Models ────────────────────────────────────────────────────────────────────

// SplitDept uses the default split mode (no explicit mode tag).
type SplitDept struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale,filterable,sortable"`
	Code string                `json:"code" db:"code" mfx:"required,unique,filterable,sortable"`
}

// ResolveDept always returns a string for Name regardless of ?locale=.
type ResolveDept struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale,resolve,filterable,sortable"`
	Code string                `json:"code" db:"code" mfx:"required,unique,filterable"`
}

// DynamicDept uses the legacy dynamic mode — string or map depending on ?locale=.
type DynamicDept struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale,dynamic,filterable,sortable"`
	Code string                `json:"code" db:"code" mfx:"required,unique,filterable"`
}

// FieldDefaultDept has a per-field default locale of "ar".
type FieldDefaultDept struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale,resolve,default_locale:ar,filterable"`
	Code string                `json:"code" db:"code" mfx:"required,unique,filterable"`
}

// SplitCustomSuffixDept uses split mode; LocaleOptions sets SplitSuffix="_translations".
type SplitCustomSuffixDept struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale,filterable"`
	Code string                `json:"code" db:"code" mfx:"required,unique,filterable"`
}

// ── Server helpers ─────────────────────────────────────────────────────────────

func splitServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{SplitDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported:  []string{"en", "ar"},
				Default:    "en",
				FromHeader: true,
				RTL:        []string{"ar"},
			}))
			s.Pipeline.Validate.Register(
				validate.RequireLocale("name", "en"),
				maniflex.ForModel("SplitDept"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})
}

func resolveServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{ResolveDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported:  []string{"en", "ar"},
				Default:    "en",
				FromHeader: true,
				RTL:        []string{"ar"},
			}))
		},
	})
}

func dynamicServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{DynamicDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported:  []string{"en", "ar"},
				Default:    "en",
				FromHeader: true,
				RTL:        []string{"ar"},
			}))
		},
	})
}

func fieldDefaultServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{FieldDefaultDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported: []string{"en", "ar"},
				Default:   "en",
			}))
		},
	})
}

func customSuffixServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{SplitCustomSuffixDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported:   []string{"en", "ar"},
				Default:     "en",
				SplitSuffix: "_translations",
			}))
		},
	})
}

// ── Helper ────────────────────────────────────────────────────────────────────

func assertString(t *testing.T, label string, got any, want string) {
	t.Helper()
	s, ok := got.(string)
	if !ok {
		t.Fatalf("%s: expected string, got %T: %v", label, got, got)
	}
	if s != want {
		t.Errorf("%s: got %q, want %q", label, s, want)
	}
}

func assertMap(t *testing.T, label string, got any) map[string]any {
	t.Helper()
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("%s: expected map[string]any, got %T: %v", label, got, got)
	}
	return m
}

// ── Split mode ────────────────────────────────────────────────────────────────

func TestSplit_CreateReturnsBothFields(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	data := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Finance", "ar": "مالية"},
		"code": "FIN",
	}).AssertStatus(http.StatusCreated).Data()

	// name is the resolved string (default locale "en")
	assertString(t, "name", data["name"], "Finance")

	// name_i18n is always the full map
	m := assertMap(t, "name_i18n", data["name_i18n"])
	if m["en"] != "Finance" {
		t.Errorf("name_i18n.en: got %v, want Finance", m["en"])
	}
	if m["ar"] != "مالية" {
		t.Errorf("name_i18n.ar: got %v, want مالية", m["ar"])
	}
}

func TestSplit_ReadWithLocaleParam(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "HR", "ar": "الموارد البشرية"},
		"code": "HR",
	}).AssertStatus(http.StatusCreated).ID()

	data := srv.GET("/split_depts/" + id + "?locale=ar").AssertStatus(http.StatusOK).Data()

	// name collapses to the Arabic string
	assertString(t, "name with ?locale=ar", data["name"], "الموارد البشرية")

	// name_i18n is still the full map regardless of ?locale=
	m := assertMap(t, "name_i18n", data["name_i18n"])
	if m["en"] != "HR" {
		t.Errorf("name_i18n.en: got %v, want HR", m["en"])
	}
}

func TestSplit_ReadWithNoLocale_ResolvesToDefault(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Operations", "ar": "العمليات"},
		"code": "OPS",
	}).AssertStatus(http.StatusCreated).ID()

	// No ?locale= — name resolves to app default "en"
	data := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()
	assertString(t, "name without locale", data["name"], "Operations")
	assertMap(t, "name_i18n", data["name_i18n"])
}

func TestSplit_ListWithLocaleParam(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Accounting", "ar": "المحاسبة"},
		"code": "ACC",
	}).AssertStatus(http.StatusCreated)

	items := srv.GET("/split_depts?locale=ar").AssertStatus(http.StatusOK).DataList()
	if len(items) == 0 {
		t.Fatal("expected at least one item")
	}
	first := assertMap(t, "items[0]", items[0])
	assertString(t, "name with ?locale=ar", first["name"], "المحاسبة")
	assertMap(t, "name_i18n", first["name_i18n"])
}

func TestSplit_RTLMetaInListResponse(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Admin", "ar": "الإدارة"},
		"code": "ADM",
	}).AssertStatus(http.StatusCreated)

	meta := srv.GET("/split_depts?locale=ar").AssertStatus(http.StatusOK).Meta()
	if meta["_dir"] != "rtl" {
		t.Errorf("meta._dir with locale=ar: got %v, want rtl", meta["_dir"])
	}
}

func TestSplit_NoRTLMetaForLTR(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Support"},
		"code": "SUP",
	}).AssertStatus(http.StatusCreated)

	meta := srv.GET("/split_depts?locale=en").AssertStatus(http.StatusOK).Meta()
	if dir, ok := meta["_dir"]; ok && dir != "" {
		t.Errorf("meta._dir with locale=en: expected absent/empty, got %v", dir)
	}
}

func TestSplit_MissingLocaleKeyFallsBackToDefault(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	// Record only has "en" — no "ar"
	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Security"},
		"code": "SEC",
	}).AssertStatus(http.StatusCreated).ID()

	// ?locale=ar requested but key missing — falls back through chain to "en"
	data := srv.GET("/split_depts/" + id + "?locale=ar").AssertStatus(http.StatusOK).Data()
	assertString(t, "name fallback", data["name"], "Security")
}

func TestSplit_UnsupportedLocaleResolvesToDefault(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Legal", "ar": "القانونية"},
		"code": "LGL",
	}).AssertStatus(http.StatusCreated).ID()

	// ?locale=fr is not in Supported — ctx.Locale stays ""; name resolves to app default "en"
	data := srv.GET("/split_depts/" + id + "?locale=fr").AssertStatus(http.StatusOK).Data()
	assertString(t, "name with unsupported locale", data["name"], "Legal")
	// i18n map still has all keys
	m := assertMap(t, "name_i18n", data["name_i18n"])
	if m["ar"] != "القانونية" {
		t.Errorf("name_i18n.ar: got %v, want القانونية", m["ar"])
	}
}

func TestSplit_AcceptLanguageHeader(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Sales", "ar": "المبيعات"},
		"code": "SLS",
	}).AssertStatus(http.StatusCreated).ID()

	data := srv.GET("/split_depts/"+id, map[string]string{"Accept-Language": "ar"}).AssertStatus(http.StatusOK).Data()
	assertString(t, "name via Accept-Language: ar", data["name"], "المبيعات")
}

func TestSplit_QueryParamOverridesHeader(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Marketing", "ar": "التسويق"},
		"code": "MKT",
	}).AssertStatus(http.StatusCreated).ID()

	// ?locale=en overrides Accept-Language: ar
	data := srv.GET("/split_depts/"+id+"?locale=en", map[string]string{"Accept-Language": "ar"}).AssertStatus(http.StatusOK).Data()
	assertString(t, "name with ?locale=en override", data["name"], "Marketing")
}

func TestSplit_Update(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "IT", "ar": "تقنية المعلومات"},
		"code": "IT",
	}).AssertStatus(http.StatusCreated).ID()

	srv.PATCH("/split_depts/"+id, map[string]any{
		"name": map[string]any{"en": "Information Technology", "ar": "تقنية المعلومات"},
	}).AssertStatus(http.StatusOK)

	data := srv.GET("/split_depts/" + id + "?locale=en").AssertStatus(http.StatusOK).Data()
	assertString(t, "updated name", data["name"], "Information Technology")
}

func TestSplit_FilterByLocaleSubkey(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Pharmacy", "ar": "الصيدلية"},
		"code": "PHR",
	}).AssertStatus(http.StatusCreated)
	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Radiology", "ar": "الأشعة"},
		"code": "RAD",
	}).AssertStatus(http.StatusCreated)

	items := srv.GET("/split_depts?filter=name.ar:ilike:%D8%A7%D9%84%D8%B5%D9%8A%D8%AF%D9%84%D9%8A%D8%A9").
		AssertStatus(http.StatusOK).DataList()
	if len(items) != 1 {
		t.Errorf("filter name.ar=الصيدلية: got %d items, want 1", len(items))
	}
}

func TestSplit_FlatFilterAutoExpandsToEffectiveLocale(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Cardiology", "ar": "أمراض القلب"},
		"code": "CAR",
	}).AssertStatus(http.StatusCreated)
	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Neurology", "ar": "الأعصاب"},
		"code": "NEU",
	}).AssertStatus(http.StatusCreated)

	// No locale sub-key — auto-expands to name.{effective locale} = name.en (app default)
	items := srv.GET("/split_depts?filter=name:eq:Cardiology").AssertStatus(http.StatusOK).DataList()
	if len(items) != 1 {
		t.Errorf("flat filter name=Cardiology: got %d items, want 1", len(items))
	}
}

func TestSplit_FlatFilterWithExplicitLocale(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Oncology", "ar": "علم الأورام"},
		"code": "ONC",
	}).AssertStatus(http.StatusCreated)

	// ?locale=ar + flat filter → auto-expands to name.ar
	items := srv.GET("/split_depts?locale=ar&filter=name:eq:%D8%B9%D9%84%D9%85+%D8%A7%D9%84%D8%A3%D9%88%D8%B1%D8%A7%D9%85").
		AssertStatus(http.StatusOK).DataList()
	if len(items) != 1 {
		t.Errorf("flat filter with ?locale=ar: got %d items, want 1", len(items))
	}
}

func TestSplit_SortByLocaleFieldUsesEffectiveLocale(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	for _, pair := range [][2]string{{"Bravo", "BST"}, {"Alpha", "AST"}, {"Charlie", "CST"}} {
		srv.POST("/split_depts", map[string]any{
			"name": map[string]any{"en": pair[0]},
			"code": pair[1],
		}).AssertStatus(http.StatusCreated)
	}

	// sort=name:asc with no ?locale= → sorts by app default "en"
	items := srv.GET("/split_depts?sort=name:asc").AssertStatus(http.StatusOK).DataList()
	if len(items) < 3 {
		t.Fatalf("expected at least 3 items, got %d", len(items))
	}
	names := make([]string, len(items))
	for i, item := range items {
		m := assertMap(t, "item", item)
		names[i], _ = m["name"].(string)
	}
	if names[0] != "Alpha" || names[1] != "Bravo" || names[2] != "Charlie" {
		t.Errorf("sort order: got %v, want [Alpha Bravo Charlie]", names[:3])
	}
}

func TestSplit_CustomSuffix(t *testing.T) {
	t.Parallel()
	srv := customSuffixServer(t)

	data := srv.POST("/split_custom_suffix_depts", map[string]any{
		"name": map[string]any{"en": "Billing", "ar": "الفواتير"},
		"code": "BIL",
	}).AssertStatus(http.StatusCreated).Data()

	// companion field uses the configured suffix, not "_i18n"
	if _, hasI18n := data["name_i18n"]; hasI18n {
		t.Error("name_i18n should not exist when SplitSuffix is _translations")
	}
	m := assertMap(t, "name_translations", data["name_translations"])
	if m["en"] != "Billing" {
		t.Errorf("name_translations.en: got %v, want Billing", m["en"])
	}
}

func TestSplit_I18nFieldIgnoredOnWrite(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	// Sending name_i18n on write should be silently ignored
	data := srv.POST("/split_depts", map[string]any{
		"name":     map[string]any{"en": "Pathology"},
		"name_i18n": map[string]any{"en": "IGNORED", "ar": "IGNORED"},
		"code":     "PTH",
	}).AssertStatus(http.StatusCreated).Data()

	assertString(t, "name", data["name"], "Pathology")
	m := assertMap(t, "name_i18n", data["name_i18n"])
	if m["en"] != "Pathology" {
		t.Errorf("name_i18n.en: got %v, want Pathology (not the injected IGNORED value)", m["en"])
	}
}

// ── Resolve mode ──────────────────────────────────────────────────────────────

func TestResolve_AlwaysString(t *testing.T) {
	t.Parallel()
	srv := resolveServer(t)

	data := srv.POST("/resolve_depts", map[string]any{
		"name": map[string]any{"en": "Finance", "ar": "مالية"},
		"code": "FIN",
	}).AssertStatus(http.StatusCreated).Data()

	// always a string — no _i18n companion
	assertString(t, "name", data["name"], "Finance")
	if _, ok := data["name_i18n"]; ok {
		t.Error("name_i18n should not exist in resolve mode")
	}
}

func TestResolve_WithLocaleParam(t *testing.T) {
	t.Parallel()
	srv := resolveServer(t)

	id := srv.POST("/resolve_depts", map[string]any{
		"name": map[string]any{"en": "HR", "ar": "الموارد البشرية"},
		"code": "HR",
	}).AssertStatus(http.StatusCreated).ID()

	data := srv.GET("/resolve_depts/" + id + "?locale=ar").AssertStatus(http.StatusOK).Data()
	assertString(t, "name with ?locale=ar", data["name"], "الموارد البشرية")
}

func TestResolve_NoLocale_UsesAppDefault(t *testing.T) {
	t.Parallel()
	srv := resolveServer(t)

	id := srv.POST("/resolve_depts", map[string]any{
		"name": map[string]any{"en": "Operations", "ar": "العمليات"},
		"code": "OPS",
	}).AssertStatus(http.StatusCreated).ID()

	// No ?locale= — resolves to app default "en"
	data := srv.GET("/resolve_depts/" + id).AssertStatus(http.StatusOK).Data()
	assertString(t, "name without locale", data["name"], "Operations")
}

func TestResolve_MissingKeyFallsBack(t *testing.T) {
	t.Parallel()
	srv := resolveServer(t)

	// Only "en" stored — no "ar"
	id := srv.POST("/resolve_depts", map[string]any{
		"name": map[string]any{"en": "Security"},
		"code": "SEC",
	}).AssertStatus(http.StatusCreated).ID()

	// Request "ar" → missing → falls back to app default "en"
	data := srv.GET("/resolve_depts/" + id + "?locale=ar").AssertStatus(http.StatusOK).Data()
	assertString(t, "name fallback", data["name"], "Security")
}

func TestResolve_FieldDefaultLocale(t *testing.T) {
	t.Parallel()
	srv := fieldDefaultServer(t)

	id := srv.POST("/field_default_depts", map[string]any{
		"name": map[string]any{"en": "Billing", "ar": "الفواتير"},
		"code": "BIL",
	}).AssertStatus(http.StatusCreated).ID()

	// No ?locale= — field default_locale:ar takes precedence over app default "en"
	data := srv.GET("/field_default_depts/" + id).AssertStatus(http.StatusOK).Data()
	assertString(t, "name with field default ar", data["name"], "الفواتير")
}

func TestResolve_FieldDefaultFallsBackWhenKeyMissing(t *testing.T) {
	t.Parallel()
	srv := fieldDefaultServer(t)

	// Only "en" — field default is "ar" but it's missing
	id := srv.POST("/field_default_depts", map[string]any{
		"name": map[string]any{"en": "Logistics"},
		"code": "LOG",
	}).AssertStatus(http.StatusCreated).ID()

	// chain: ctx.Locale="" → field_default="ar" (missing) → app_default="en" ✓
	data := srv.GET("/field_default_depts/" + id).AssertStatus(http.StatusOK).Data()
	assertString(t, "name fallback from field default", data["name"], "Logistics")
}

// ── Dynamic mode ──────────────────────────────────────────────────────────────

func TestDynamic_WithLocaleParam_ReturnsString(t *testing.T) {
	t.Parallel()
	srv := dynamicServer(t)

	id := srv.POST("/dynamic_depts", map[string]any{
		"name": map[string]any{"en": "Finance", "ar": "مالية"},
		"code": "FIN",
	}).AssertStatus(http.StatusCreated).ID()

	data := srv.GET("/dynamic_depts/" + id + "?locale=ar").AssertStatus(http.StatusOK).Data()
	assertString(t, "name with ?locale=ar", data["name"], "مالية")
}

func TestDynamic_WithoutLocaleParam_ReturnsFullMap(t *testing.T) {
	t.Parallel()
	srv := dynamicServer(t)

	id := srv.POST("/dynamic_depts", map[string]any{
		"name": map[string]any{"en": "HR", "ar": "الموارد البشرية"},
		"code": "HR",
	}).AssertStatus(http.StatusCreated).ID()

	// No ?locale= → full map
	data := srv.GET("/dynamic_depts/" + id).AssertStatus(http.StatusOK).Data()
	m := assertMap(t, "name without locale", data["name"])
	if m["en"] != "HR" {
		t.Errorf("name.en: got %v, want HR", m["en"])
	}
	if m["ar"] != "الموارد البشرية" {
		t.Errorf("name.ar: got %v, want الموارد البشرية", m["ar"])
	}
	// no _i18n companion in dynamic mode
	if _, ok := data["name_i18n"]; ok {
		t.Error("name_i18n should not exist in dynamic mode")
	}
}

func TestDynamic_FlatFilterWithNoLocale_HitsJsonColumn(t *testing.T) {
	t.Parallel()
	if testutil.IsPostgres() {
		// This asserts SQLite's TEXT-JSON column semantics: a flat filter on a
		// locale field with no ?locale= compares `name = 'Oncology'` against the
		// raw JSON column, which on SQLite's TEXT storage simply matches nothing
		// (0 rows). On Postgres the column is a native json type and `json = text`
		// is a hard type error (22P02), not an empty result — so the "0 matches"
		// expectation is inherently SQLite-specific.
		t.Skip("asserts SQLite TEXT-JSON column comparison semantics; Postgres json = text errors")
	}
	srv := dynamicServer(t)

	srv.POST("/dynamic_depts", map[string]any{
		"name": map[string]any{"en": "Oncology"},
		"code": "ONC",
	}).AssertStatus(http.StatusCreated)

	// In dynamic mode with no ?locale=, flat filter is not auto-expanded
	// and hits the raw JSON column — won't match a plain string value, so 0 results.
	items := srv.GET("/dynamic_depts?filter=name:eq:Oncology").AssertStatus(http.StatusOK).DataList()
	if len(items) != 0 {
		t.Errorf("dynamic flat filter without locale: expected 0 matches (JSON column), got %d", len(items))
	}
}

func TestDynamic_FlatFilterWithLocale_AutoExpands(t *testing.T) {
	t.Parallel()
	srv := dynamicServer(t)

	srv.POST("/dynamic_depts", map[string]any{
		"name": map[string]any{"en": "Nephrology", "ar": "أمراض الكلى"},
		"code": "NEP",
	}).AssertStatus(http.StatusCreated)

	// dynamic + ?locale=en → flat filter auto-expands to name.en
	items := srv.GET("/dynamic_depts?locale=en&filter=name:eq:Nephrology").AssertStatus(http.StatusOK).DataList()
	if len(items) != 1 {
		t.Errorf("dynamic flat filter with ?locale=en: got %d items, want 1", len(items))
	}
}

// ── validate.RequireLocale ────────────────────────────────────────────────────

func TestRequireLocale_MissingKey(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	resp := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"ar": "فقط عربي"},
		"code": "XYZ",
	})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "MISSING_LOCALE" {
		t.Errorf("error code: got %q, want MISSING_LOCALE", code)
	}
}

func TestRequireLocale_PassesWhenKeyPresent(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Oncology"},
		"code": "ONC2",
	}).AssertStatus(http.StatusCreated)
}

// ── RTL meta on single-record responses ───────────────────────────────────────

func TestSplit_RTLMetaInReadResponse(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	id := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Finance", "ar": "مالية"},
		"code": "FINRTL",
	}).AssertStatus(http.StatusCreated).ID()

	// Single-record GET with RTL locale must carry meta._dir: rtl.
	meta := srv.GET("/split_depts/"+id+"?locale=ar").AssertStatus(http.StatusOK).Meta()
	if meta["_dir"] != "rtl" {
		t.Errorf("meta._dir on GET single record with locale=ar: got %v, want rtl", meta["_dir"])
	}
}

func TestSplit_RTLMetaInCreateResponse(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	// POST with Accept-Language: ar — create response must carry meta._dir: rtl.
	meta := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Payroll", "ar": "كشف الرواتب"},
		"code": "PAYRTL",
	}, map[string]string{"Accept-Language": "ar"}).AssertStatus(http.StatusCreated).Meta()
	if meta["_dir"] != "rtl" {
		t.Errorf("meta._dir on POST with Accept-Language: ar: got %v, want rtl", meta["_dir"])
	}
}

// ── Dynamic sort without locale ───────────────────────────────────────────────

func TestDynamic_SortWithoutLocale_UsesRawColumn(t *testing.T) {
	t.Parallel()
	srv := dynamicServer(t)

	for _, pair := range [][2]string{{"Bravo", "BST2"}, {"Alpha", "AST2"}, {"Charlie", "CST2"}} {
		srv.POST("/dynamic_depts", map[string]any{
			"name": map[string]any{"en": pair[0]},
			"code": pair[1],
		}).AssertStatus(http.StatusCreated)
	}

	// dynamic mode + no ?locale= → sort is on the raw JSON column, not a locale
	// sub-key, so the order is non-alphabetical by English name. The response
	// must still succeed (200) and return all three rows regardless of order.
	items := srv.GET("/dynamic_depts?sort=name:asc").AssertStatus(http.StatusOK).DataList()
	if len(items) < 3 {
		t.Errorf("dynamic sort without locale: expected ≥3 items, got %d", len(items))
	}
	// The full map is returned per item (no locale= set).
	first := assertMap(t, "items[0]", items[0])
	assertMap(t, "items[0].name should be a map in dynamic mode", first["name"])
}

func TestRequireLocale_EmptyValueRejected(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	resp := srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "", "ar": "شيء ما"},
		"code": "EMP",
	})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "MISSING_LOCALE" {
		t.Errorf("error code: got %q, want MISSING_LOCALE", code)
	}
}
