package e2e

// Audit STEP-6 — a body value the model's Go type cannot hold was written to
// the column verbatim, and on SQLite the typed scan on the way back out then
// failed the whole result set: one such row answered 500 on every list and
// every read of that collection, for every caller, until someone found it by
// id and repaired it. Postgres refused the INSERT and surfaced an opaque 500.
//
// Deserialize decodes the body into ctx.Record best-effort — "a decode mismatch
// here never fails the request" — so the mismatch left ctx.Record nil, the DB
// step sourced the write from ParsedBody, and Validate, which knows the tags but
// not the types, had nothing to say about it.
//
// Both doors are covered here. JSON carries types, so json itself decides. A
// multipart form carries only strings, so those are converted to the column's
// type and only the ones that will not convert are refused — which is the door
// that opened by accident: a blank <input type="number"> posts "".
//
// Every case asserts the collection still lists afterwards. That is the finding:
// not that one request was wrong, but that one request took the model down.

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/money"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type TypedDoc struct {
	maniflex.BaseModel
	Name  string     `json:"name"  db:"name"  mfx:"required"`
	Age   int        `json:"age"   db:"age"`
	Opt   *int       `json:"opt"   db:"opt"`
	Score float64    `json:"score" db:"score"`
	OK    bool       `json:"ok"    db:"ok"`
	When  *time.Time `json:"when"  db:"when"`
	Code  string     `json:"code"  db:"code"`
	Owner string     `json:"owner" db:"owner" mfx:"readonly"`
}

// typedSrv seeds one good row, so every assertion below is about what a second
// request does to the collection the first one is in.
func typedSrv(t *testing.T) (*testutil.Server, string) {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{TypedDoc{}}})
	id := srv.MustID(srv.POST("/typed_docs", map[string]any{"name": "good", "age": 3}))
	return srv, id
}

// assertCollectionReadable is the point of the finding: the row that was refused
// must not have been able to take the endpoint down on its way in.
func assertCollectionReadable(t *testing.T, srv *testutil.Server, goodID string) {
	t.Helper()
	if resp := srv.GET("/typed_docs"); resp.Status != http.StatusOK {
		t.Errorf("listing the collection answers %d after the refused write; the row "+
			"must not have reached the column\nbody: %s", resp.Status, resp.Body)
	}
	if resp := srv.GET("/typed_docs/" + goodID); resp.Status != http.StatusOK {
		t.Errorf("reading an unrelated row answers %d", resp.Status)
	}
}

// detailFor returns the message reported for one field, or "" when the response
// carries no entry for it.
func detailFor(t *testing.T, resp *testutil.Response, field string) string {
	t.Helper()
	for _, d := range detailsOf(t, resp) {
		if d["field"] == field {
			msg, _ := d["message"].(string)
			return msg
		}
	}
	return ""
}

func TestBodyTyping_JSONWrongScalarTypeIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		body  map[string]any
		field string
		want  string
	}{
		{"string into int", map[string]any{"name": "w", "age": "abc"}, "age", "must be a whole number"},
		{"fraction into int", map[string]any{"name": "w", "age": 1.5}, "age", "must be a whole number"},
		{"object into int", map[string]any{"name": "w", "age": map[string]any{"a": 1}}, "age", "must be a whole number"},
		{"string into float", map[string]any{"name": "w", "score": "xyz"}, "score", "must be a number"},
		{"string into bool", map[string]any{"name": "w", "ok": "yes"}, "ok", "must be true or false"},
		{"number into string", map[string]any{"name": 42}, "name", "must be a string"},
		{"junk into time", map[string]any{"name": "w", "when": "not-a-date"}, "when", "must be an RFC 3339 timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, goodID := typedSrv(t)

			resp := srv.POST("/typed_docs", tc.body)
			resp.AssertStatus(http.StatusUnprocessableEntity)
			if got := detailFor(t, resp, tc.field); got != tc.want {
				t.Errorf("details for %q = %q, want %q\nbody: %s", tc.field, got, tc.want, resp.Body)
			}
			assertCollectionReadable(t, srv, goodID)
		})
	}
}

// json stops recording after the first type error, so a single whole-body decode
// could only ever name one field. Validate reports every rule it checks, and
// this one has to answer in the same shape.
func TestBodyTyping_JSONReportsEveryBadField(t *testing.T) {
	t.Parallel()
	srv, goodID := typedSrv(t)

	resp := srv.POST("/typed_docs", map[string]any{
		"name": "w", "age": "abc", "score": "xyz", "ok": "maybe",
	})
	resp.AssertStatus(http.StatusUnprocessableEntity)

	for _, f := range []string{"age", "score", "ok"} {
		if detailFor(t, resp, f) == "" {
			t.Errorf("no detail for %q; a client fixing its form needs all of them at "+
				"once\nbody: %s", f, resp.Body)
		}
	}
	assertCollectionReadable(t, srv, goodID)
}

// The same rule on the update door, which writes the same columns.
func TestBodyTyping_JSONUpdateIsRefusedToo(t *testing.T) {
	t.Parallel()
	srv, goodID := typedSrv(t)

	srv.PATCH("/typed_docs/"+goodID, map[string]any{"age": "abc"}).
		AssertStatus(http.StatusUnprocessableEntity)

	assertCollectionReadable(t, srv, goodID)
	if got := srv.GET("/typed_docs/" + goodID).Data()["age"]; got != float64(3) {
		t.Errorf("age = %#v after the refused PATCH, want the original 3", got)
	}
}

// A well-typed body must be untouched by any of this, values and all — the check
// runs on the way to the same write it always did.
func TestBodyTyping_WellTypedBodyIsUnaffected(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{TypedDoc{}}})

	resp := srv.POST("/typed_docs", map[string]any{
		"name": "w", "age": 7, "opt": 9, "score": 1.5, "ok": true,
		"when": "2020-01-02T03:04:05Z", "code": "01234",
	})
	resp.AssertStatus(http.StatusCreated)

	got := srv.GET("/typed_docs/" + resp.ID()).Data()
	for _, tc := range []struct {
		key  string
		want any
	}{
		{"name", "w"}, {"age", float64(7)}, {"opt", float64(9)},
		{"score", 1.5}, {"ok", true}, {"code", "01234"},
	} {
		if got[tc.key] != tc.want {
			t.Errorf("%s = %#v, want %#v", tc.key, got[tc.key], tc.want)
		}
	}
	if got["when"] == nil {
		t.Errorf("when was not stored: %#v", got)
	}
}

// Validate strips readonly before it reads anything else, so a value that was
// never going to be written must not produce a type error about itself. The
// order matters: reporting it would tell a client to fix a field the server
// ignores.
func TestBodyTyping_StrippedFieldsReportNothing(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{TypedDoc{}}})

	resp := srv.POST("/typed_docs", map[string]any{"name": "w", "owner": 42})
	resp.AssertStatus(http.StatusCreated)
	if got := resp.Data()["owner"]; got != "" {
		t.Errorf("owner = %#v, want empty — readonly is stripped, not stored", got)
	}
}

func TestBodyTyping_FormValuesAreConverted(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{TypedDoc{}}})

	resp := srv.POSTMultipart("/typed_docs", map[string]string{
		"name": "m", "age": "7", "score": "1.5", "ok": "on",
		"when": "2020-01-02T03:04:05Z", "code": "01234",
	}, nil)
	resp.AssertStatus(http.StatusCreated)

	got := srv.GET("/typed_docs/" + resp.ID()).Data()
	if got["age"] != float64(7) || got["score"] != 1.5 || got["ok"] != true {
		t.Errorf("form values did not reach the columns as their own types: %#v", got)
	}
	// A form value for a string column is already the right shape and must not
	// be "helpfully" read as a number.
	if got["code"] != "01234" {
		t.Errorf("code = %#v, want %q", got["code"], "01234")
	}
}

func TestBodyTyping_FormJunkIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key, val, want string }{
		{"not a number", "age", "abc", "must be a whole number"},
		{"not a boolean", "ok", "maybe", "must be true or false"},
		{"date without a time", "when", "2020-01-01", "must be an RFC 3339 timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, goodID := typedSrv(t)

			resp := srv.POSTMultipart("/typed_docs", map[string]string{"name": "m", tc.key: tc.val}, nil)
			resp.AssertStatus(http.StatusUnprocessableEntity)
			if got := detailFor(t, resp, tc.key); got != tc.want {
				t.Errorf("details for %q = %q, want %q\nbody: %s", tc.key, got, tc.want, resp.Body)
			}
			assertCollectionReadable(t, srv, goodID)
		})
	}
}

// The door that opens without anyone trying: a browser posts every input in the
// form, so an untouched number field arrives as "". A nullable column takes the
// null; one whose type has no null is refused by name, which is the answer the
// same body already got as JSON (audit MS-7).
func TestBodyTyping_EmptyFormValueIsNoValue(t *testing.T) {
	t.Parallel()
	srv, goodID := typedSrv(t)

	blank := srv.POSTMultipart("/typed_docs", map[string]string{"name": "m", "opt": ""}, nil)
	blank.AssertStatus(http.StatusCreated)
	if got := srv.GET("/typed_docs/" + blank.ID()).Data()["opt"]; got != nil {
		t.Errorf("opt = %#v for a blank optional input, want null", got)
	}

	resp := srv.POSTMultipart("/typed_docs", map[string]string{"name": "m", "age": ""}, nil)
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if got := detailFor(t, resp, "age"); got == "" {
		t.Errorf("a blank non-nullable number reported nothing\nbody: %s", resp.Body)
	}
	assertCollectionReadable(t, srv, goodID)
}

// ── Types that own their own representation ──────────────────────────────────

type PricedDoc struct {
	maniflex.BaseModel
	Name  string       `json:"name"  db:"name"  mfx:"required"`
	Price money.Amount `json:"price" db:"price"`
}

// money.Amount has two doors — the {amount, currency} object its UnmarshalJSON
// reads, and a bare decimal that reaches the column through the map path and
// comes back through Scan. Both work today and the check must not close either;
// what it closes is the value neither accepts.
func TestBodyTyping_SQLTyperKeepsBothItsShapes(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{PricedDoc{}}})

	bare := srv.POST("/priced_docs", map[string]any{"name": "a", "price": 12.34})
	bare.AssertStatus(http.StatusCreated)

	obj := srv.POST("/priced_docs", map[string]any{
		"name": "b", "price": map[string]any{"amount": 500, "currency": "USD"},
	})
	obj.AssertStatus(http.StatusCreated)

	srv.GET("/priced_docs").AssertStatus(http.StatusOK)
}

func TestBodyTyping_SQLTyperRefusesWhatNeitherDoorAccepts(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{PricedDoc{}}})
	good := srv.MustID(srv.POST("/priced_docs", map[string]any{"name": "a", "price": 12.34}))

	srv.POST("/priced_docs", map[string]any{"name": "b", "price": "abc"}).
		AssertStatus(http.StatusUnprocessableEntity)
	srv.POSTMultipart("/priced_docs", map[string]string{"name": "c", "price": "abc"}, nil).
		AssertStatus(http.StatusUnprocessableEntity)

	if resp := srv.GET("/priced_docs"); resp.Status != http.StatusOK {
		t.Errorf("listing answers %d; the refused values must not have been stored\nbody: %s",
			resp.Status, resp.Body)
	}
	if resp := srv.GET("/priced_docs/" + good); resp.Status != http.StatusOK {
		t.Errorf("reading the good row answers %d", resp.Status)
	}
}

// ── Locale columns ───────────────────────────────────────────────────────────

type LocalisedDoc struct {
	maniflex.BaseModel
	Title maniflex.LocaleString `json:"title" db:"title" mfx:"locale"`
}

// LocaleString.UnmarshalJSON accepts only a map, and split mode answers a read
// with a bare string — so echoing a response back fails the decode on purpose,
// which is what routes the write through localeWriteValue. Refusing that here
// would break the documented round-trip rather than catch anything.
func TestBodyTyping_LocaleBareStringStillWrites(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{LocalisedDoc{}}})

	resp := srv.POST("/localised_docs", map[string]any{"title": "Cardiology"})
	resp.AssertStatus(http.StatusCreated)

	if got := srv.GET("/localised_docs/" + resp.ID()); got.Status != http.StatusOK {
		t.Errorf("reading the record answers %d\nbody: %s", got.Status, got.Body)
	}
}
