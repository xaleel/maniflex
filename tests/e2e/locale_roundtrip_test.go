package e2e

// MS-14: split mode answers a read with "name" (the resolved string for one
// locale) plus "name_i18n" (the full map), and the write side understood
// neither. A client echoing that response back sent "name":"Cardiology" into a
// locale column, which was stored as a JSON scalar — after which the row could
// not be scanned at all: 500 on the record, and 500 on the whole collection,
// because one bad row fails the list scan.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func createCardiology(t *testing.T, srv *testutil.Server) (id string, created map[string]any) {
	t.Helper()
	created = srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Cardiology", "ar": "أمراض القلب"},
		"code": "CARD",
	}).AssertStatus(http.StatusCreated).Data()
	return created["id"].(string), created
}

// TestLocaleRoundTrip_EchoingTheResponseIsLossless is the headline case: a
// generic edit form GETs a record, changes one unrelated field and PATCHes the
// whole object back. Every translation must survive, including the ones the
// response only showed inside name_i18n.
func TestLocaleRoundTrip_EchoingTheResponseIsLossless(t *testing.T) {
	t.Parallel()

	srv := splitServer(t)
	id, created := createCardiology(t, srv)

	echo := map[string]any{
		"name":      created["name"],      // the resolved string
		"name_i18n": created["name_i18n"], // the full map
		"code":      "CARD2",
	}
	srv.PATCH("/split_depts/"+id, echo).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()
	i18n, ok := got["name_i18n"].(map[string]any)
	if !ok {
		t.Fatalf("name_i18n missing after round-trip: %#v", got["name_i18n"])
	}
	if i18n["en"] != "Cardiology" {
		t.Errorf("en lost: got %#v", i18n["en"])
	}
	// The one the response never showed as a bare string — dropping it is the
	// silent data loss a coerce-only fix would still have caused.
	if i18n["ar"] != "أمراض القلب" {
		t.Errorf("ar lost in round-trip: got %#v", i18n["ar"])
	}
	if got["name"] != "Cardiology" {
		t.Errorf("resolved name: got %#v", got["name"])
	}
}

// TestLocaleRoundTrip_ListSurvives pins the blast radius. The failure was never
// confined to one field or one record: the scan aborted the row, so a single
// bad PATCH denied the entire collection endpoint to every caller.
func TestLocaleRoundTrip_ListSurvives(t *testing.T) {
	t.Parallel()

	srv := splitServer(t)
	id, created := createCardiology(t, srv)
	srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Neurology"}, "code": "NEUR",
	}).AssertStatus(http.StatusCreated)

	// The lossy echo: bare string only, no companion.
	srv.PATCH("/split_depts/"+id, map[string]any{"name": created["name"]}).
		AssertStatus(http.StatusOK)

	rows := srv.GET("/split_depts").AssertStatus(http.StatusOK).DataList()
	if len(rows) != 2 {
		t.Fatalf("list after a bare-string write: want 2 rows, got %d", len(rows))
	}
	srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK)
}

// TestLocaleRoundTrip_BareStringFoldsToRequestLocale covers the coercion on its
// own, and that the key is the *effective* locale rather than a hardcoded one.
func TestLocaleRoundTrip_BareStringFoldsToRequestLocale(t *testing.T) {
	t.Parallel()

	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	srv.PATCH("/split_depts/"+id, map[string]any{"name": "أمراض القلب"},
		map[string]string{"Accept-Language": "ar"}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/"+id, map[string]string{"Accept-Language": "ar"}).
		AssertStatus(http.StatusOK).Data()
	i18n, ok := got["name_i18n"].(map[string]any)
	if !ok {
		t.Fatalf("name_i18n missing: %#v", got["name_i18n"])
	}
	if i18n["ar"] != "أمراض القلب" {
		t.Errorf("bare string should fold under the request locale ar: %#v", i18n)
	}
	if _, hasEN := i18n["en"]; hasEN {
		t.Errorf("bare-string write replaces the column, as a map write does; got %#v", i18n)
	}
}

// TestLocaleRoundTrip_MapWriteUnchanged is the anti-vacuity pair: a fix that
// folded or ignored indiscriminately would break the ordinary path.
func TestLocaleRoundTrip_MapWriteUnchanged(t *testing.T) {
	t.Parallel()

	srv := splitServer(t)
	id, _ := createCardiology(t, srv)

	srv.PATCH("/split_depts/"+id, map[string]any{
		"name": map[string]any{"en": "Cardio", "fr": "Cardiologie"},
	}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()
	i18n := got["name_i18n"].(map[string]any)
	if i18n["en"] != "Cardio" || i18n["fr"] != "Cardiologie" {
		t.Errorf("map write: got %#v", i18n)
	}
	if _, hasAR := i18n["ar"]; hasAR {
		t.Errorf("a map write still replaces the column: got %#v", i18n)
	}
}

// TestLocaleRoundTrip_CustomSuffixIsConsumed guards against hardcoding "_i18n"
// on the write side when LocaleOptions renamed it. Reading and writing must
// agree on the key, or split mode is broken again for these apps.
func TestLocaleRoundTrip_CustomSuffixIsConsumed(t *testing.T) {
	t.Parallel()

	srv := customSuffixServer(t)
	created := srv.POST("/split_custom_suffix_depts", map[string]any{
		"name": map[string]any{"en": "Cardiology", "ar": "أمراض القلب"},
		"code": "CARD",
	}).AssertStatus(http.StatusCreated).Data()
	id := created["id"].(string)

	srv.PATCH("/split_custom_suffix_depts/"+id, map[string]any{
		"name":              created["name"],
		"name_translations": created["name_translations"],
	}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_custom_suffix_depts/" + id).AssertStatus(http.StatusOK).Data()
	i18n, ok := got["name_translations"].(map[string]any)
	if !ok {
		t.Fatalf("name_translations missing: %#v", got)
	}
	if i18n["ar"] != "أمراض القلب" {
		t.Errorf("custom suffix not consumed on write: %#v", i18n)
	}
}

// TestLocaleRoundTrip_AlreadyCorruptRowIsReadable covers rows written before the
// fix. The write path cannot help them; without the read-side fallback they stay
// permanently unreadable and keep 500ing the list, recoverable only by hand-
// written SQL.
func TestLocaleRoundTrip_AlreadyCorruptRowIsReadable(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{SplitDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/split_depts/{id}/poison",
				Handler: func(ctx *maniflex.ServerContext) error {
					// Exactly what a pre-v0.2.5 bare-string write stored.
					if _, err := ctx.RawExec(
						`UPDATE split_depts SET name = '"Cardiology"' WHERE id = ?`,
						ctx.ResourceID); err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})

	id := srv.MustID(srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"en": "Cardiology"}, "code": "CARD",
	}))
	srv.POST("/split_depts/"+id+"/poison", map[string]any{}).AssertStatus(http.StatusOK)

	got := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()
	if got["name"] != "Cardiology" {
		t.Errorf("corrupt row should still render its value: %#v", got["name"])
	}
	srv.GET("/split_depts").AssertStatus(http.StatusOK)

	// And it must be repairable through the API rather than by hand.
	srv.PATCH("/split_depts/"+id, map[string]any{
		"name": map[string]any{"en": "Cardiology", "ar": "أمراض القلب"},
	}).AssertStatus(http.StatusOK)
	fixed := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()
	if i18n, ok := fixed["name_i18n"].(map[string]any); !ok || i18n["ar"] != "أمراض القلب" {
		t.Errorf("repair via PATCH failed: %#v", fixed["name_i18n"])
	}
}
