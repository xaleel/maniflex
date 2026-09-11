package e2e

// Audit STEP-7 — the `_i18n` companion is read by localeWriteValue, and only
// toDBMap called it. So the rule localization.md publishes — "that map,
// verbatim — the companion wins" — held only on the map write path, which a
// body reaches by accident when its bare-string "name" fails
// LocaleString.UnmarshalJSON and disables the typed record.
//
// Any body that did decode took the record path, where recordToMap emits the
// present columns and the companion names no column at all. Sent on its own it
// was a no-op on update (200, nothing written) and an empty column on create;
// sent beside a map it was silently dropped. The v0.2.5 entry that claims
// "name_i18n is now consumed on write and wins" covered one path of two.

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// i18nOf reads the companion map out of a response, failing with the whole body
// when it is not there — which is what the create case used to produce.
func i18nOf(t *testing.T, resp *testutil.Response, key string) map[string]any {
	t.Helper()
	m, ok := resp.Data()[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not a map: %s", key, resp.Body)
	}
	return m
}

func assertTranslations(t *testing.T, got map[string]any, en, ar string) {
	t.Helper()
	if got["en"] != en {
		t.Errorf("en = %#v, want %q", got["en"], en)
	}
	if got["ar"] != ar {
		t.Errorf("ar = %#v, want %q — the companion carries every translation, "+
			"including the ones a response only showed inside it", got["ar"], ar)
	}
}

// The documented contract, on its own: send the companion and that map is
// stored. It answered 200 and wrote nothing.
func TestLocaleCompanion_AloneWritesOnUpdate(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	srv.PATCH("/split_depts/"+id, map[string]any{
		"name_i18n": map[string]any{"en": "Cardiac Care", "ar": "رعاية القلب"},
	}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id)
	got.AssertStatus(http.StatusOK)
	assertTranslations(t, i18nOf(t, got, "name_i18n"), "Cardiac Care", "رعاية القلب")
	if got.Data()["name"] != "Cardiac Care" {
		t.Errorf("resolved name = %#v, want the new English value", got.Data()["name"])
	}
}

// The create half, which the finding does not mention: this was not a no-op but
// a row written with an empty column, since the companion names no model field
// and the record's present set was therefore empty.
func TestLocaleCompanion_AloneWritesOnCreate(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)

	resp := srv.POST("/split_depts", map[string]any{
		"name_i18n": map[string]any{"en": "Neurology", "ar": "الأعصاب"},
		"code":      "NEURO",
	})
	resp.AssertStatus(http.StatusCreated)
	assertTranslations(t, i18nOf(t, resp, "name_i18n"), "Neurology", "الأعصاب")

	// Read it back: the echo could be right while the column is empty.
	stored := srv.GET("/split_depts/" + resp.ID())
	stored.AssertStatus(http.StatusOK)
	assertTranslations(t, i18nOf(t, stored, "name_i18n"), "Neurology", "الأعصاب")
}

// "The companion takes precedence because it is the complete value, while name
// is one locale's rendering of it." The record path took the rendering and
// dropped the complete value.
func TestLocaleCompanion_WinsOverAMapSentBeside(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	srv.PATCH("/split_depts/"+id, map[string]any{
		"name":      map[string]any{"en": "English only"},
		"name_i18n": map[string]any{"en": "Cardiology", "ar": "أمراض القلب"},
	}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id)
	assertTranslations(t, i18nOf(t, got, "name_i18n"), "Cardiology", "أمراض القلب")
}

// A response read in one locale carries that locale's string in "name" and the
// whole map in the companion. PATCHing it back unchanged must store what was
// read — the case a generic edit form produces, and the one the map path only
// ever handled because the bare string broke the decode.
func TestLocaleCompanion_EchoedArabicResponseRoundTrips(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	read := srv.GET("/split_depts/"+id, map[string]string{"Accept-Language": "ar"})
	read.AssertStatus(http.StatusOK)

	srv.PATCH("/split_depts/"+id, map[string]any{
		"name":      read.Data()["name"],
		"name_i18n": read.Data()["name_i18n"],
		"code":      "CARD",
	}, map[string]string{"Accept-Language": "ar"}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id)
	assertTranslations(t, i18nOf(t, got, "name_i18n"), "Cardiology", "أمراض القلب")
}

// The suffix is configurable, so the write side has to read the one in force
// rather than a hard-coded "_i18n".
func TestLocaleCompanion_HonoursACustomSuffix(t *testing.T) {
	t.Parallel()
	srv := customSuffixServer(t)

	id := srv.MustID(srv.POST("/split_custom_suffix_depts", map[string]any{
		"name": map[string]any{"en": "Billing"}, "code": "BILL",
	}))

	srv.PATCH("/split_custom_suffix_depts/"+id, map[string]any{
		"name_translations": map[string]any{"en": "Billing", "ar": "الفواتير"},
	}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_custom_suffix_depts/" + id)
	assertTranslations(t, i18nOf(t, got, "name_translations"), "Billing", "الفواتير")

	// And the default suffix must not be honoured when another one is in force,
	// or the fix would have widened the contract rather than completed it.
	srv.PATCH("/split_custom_suffix_depts/"+id, map[string]any{
		"name_i18n": map[string]any{"en": "Ignored", "ar": "متجاهل"},
	}).AssertStatus(http.StatusOK)
	after := srv.GET("/split_custom_suffix_depts/" + id)
	assertTranslations(t, i18nOf(t, after, "name_translations"), "Billing", "الفواتير")
}

// ── The paths that already worked, which the overlay must not disturb ────────

// A body that never mentions the locale field leaves the column alone.
func TestLocaleCompanion_UnmentionedFieldIsUntouched(t *testing.T) {
	t.Parallel()
	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	srv.PATCH("/split_depts/"+id, map[string]any{"code": "CARD9"}).
		AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id)
	assertTranslations(t, i18nOf(t, got, "name_i18n"), "Cardiology", "أمراض القلب")
	if got.Data()["code"] != "CARD9" {
		t.Errorf("code = %#v, want the value the PATCH did send", got.Data()["code"])
	}
}

// A bare string still replaces the column with one locale's entry, and a map
// still stores that map — both documented, both unchanged.
func TestLocaleCompanion_BareStringAndMapWritesAreUnchanged(t *testing.T) {
	t.Parallel()

	t.Run("bare string", func(t *testing.T) {
		t.Parallel()
		srv := splitServer(t)
		id, _ := createCardiology(t, srv)

		srv.PATCH("/split_depts/"+id, map[string]any{"name": "Heart"}).
			AssertStatus(http.StatusOK)

		got := i18nOf(t, srv.GET("/split_depts/"+id), "name_i18n")
		if got["en"] != "Heart" {
			t.Errorf("en = %#v, want %q", got["en"], "Heart")
		}
		if _, still := got["ar"]; still {
			t.Errorf("ar survived a bare-string write: %#v — a bare string replaces "+
				"the column, which is what the docs say and what a map write does", got)
		}
	})

	t.Run("map", func(t *testing.T) {
		t.Parallel()
		srv := splitServer(t)
		id, _ := createCardiology(t, srv)

		srv.PATCH("/split_depts/"+id, map[string]any{
			"name": map[string]any{"en": "Cardio", "ar": "قلب"},
		}).AssertStatus(http.StatusOK)

		assertTranslations(t, i18nOf(t, srv.GET("/split_depts/"+id), "name_i18n"), "Cardio", "قلب")
	})
}

// ── Why the overlay, and not a fallback to the map path ──────────────────────

type localisedLedger struct {
	maniflex.BaseModel
	Name maniflex.LocaleString `json:"name" db:"name" mfx:"locale"`
	N    int64                 `json:"n"    db:"n"`
}

var ledgerN = regexp.MustCompile(`"n":\s*([0-9-]+)`)

// Dropping the whole body onto toDBMap would have fixed the companion and
// broken this: ParsedBody holds a JSON number as a float64, which stops holding
// every integer at 2^53. The overlay touches only the locale column, so an
// int64 sent in the same request still arrives exactly (audit N1).
func TestLocaleCompanion_LargeIntegerBesideItIsStillExact(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{localisedLedger{}}})

	const exact = "9007199254740993" // 2^53 + 1; float64 rounds it down
	resp := srv.POST("/localised_ledgers", []byte(
		`{"name_i18n": {"en":"Ledger","ar":"دفتر"}, "n": `+exact+`}`))
	resp.AssertStatus(http.StatusCreated)

	body := srv.GET("/localised_ledgers/" + resp.ID()).Body
	m := ledgerN.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no \"n\" in the response: %s", body)
	}
	if got := string(m[1]); got != exact {
		t.Errorf("n = %s, want %s — the locale overlay must not push the rest of "+
			"the write onto the map path", got, exact)
	}
	assertTranslations(t, i18nOf(t, srv.GET("/localised_ledgers/"+resp.ID()), "name_i18n"),
		"Ledger", "دفتر")
}
