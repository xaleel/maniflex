package e2e

// Audit MS-7: an explicit JSON null on a non-pointer field used to produce one
// of two different answers for the same request.
//
// The write path picks its source per request: when the body's column set
// exactly matches the typed record's present set the write is sourced from the
// record, where json.Unmarshal has already collapsed null into the Go zero
// value — so it stored "" and answered 200. Otherwise it fell to toDBMap, which
// carried the nil through to a column the migrator made NOT NULL (a non-pointer
// field always is), so the client got the database's own constraint violation
// as a 422. Which one you got turned on whether a middleware had left the body
// and the record's present set disagreeing.
//
// It is now refused in the Validate step, before either path runs: the column
// has no null to store, so the request cannot be honoured as sent. Nullability
// is spelled *T.
//
//	go test ./tests/e2e/... -run TestNullWrite

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type nwDoc struct {
	maniflex.BaseModel
	Name string  `json:"name" db:"name" mfx:"filterable"`
	Note string  `json:"note" db:"note" mfx:"filterable"`
	Ptr  *string `json:"ptr"  db:"ptr"  mfx:"filterable"`
	Tag  string  `json:"tag"  db:"tag"`
}

// nwServer optionally registers a middleware that forces the write onto the map
// path. It sets a model field to a value that does not fit the field's Go type,
// which is the one remaining way to make the body and the record's present set
// disagree: syncRecordField cannot represent the value, so it clears the present
// flag and the DB step falls back to toDBMap.
//
// The middleware never touches `note` or `ptr` — it only changes which source
// the write is read from, which is the whole point.
func nwServer(t *testing.T, mapPath bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{nwDoc{}},
		Middleware: func(s *maniflex.Server) {
			if !mapPath {
				return
			}
			s.Pipeline.Deserialize.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.SetField("tag", 12345) // an int for a string field
					return next()
				}, maniflex.ForModel("nwDoc"), maniflex.AtPosition(maniflex.After))
		},
	})
}

func nwIsNull(t *testing.T, srv *testutil.Server, col, id string) bool {
	t.Helper()
	for _, it := range srv.GET("/nw_docs?filter=" + col + ":is_null").DataList() {
		if it.(map[string]any)["id"] == id {
			return true
		}
	}
	return false
}

// assertNullRefused checks the answer is the deterministic validation error and
// not the database's constraint violation leaking through.
func assertNullRefused(t *testing.T, resp *testutil.Response, field string) {
	t.Helper()
	resp.AssertStatus(http.StatusUnprocessableEntity)
	body := string(resp.Body)
	if !strings.Contains(body, "VALIDATION_ERROR") {
		t.Errorf("want VALIDATION_ERROR, got: %s", body)
	}
	if !strings.Contains(body, field) {
		t.Errorf("the error must name the field %q: %s", field, body)
	}
	if !strings.Contains(body, "pointer") {
		t.Errorf("the error should say how to allow null: %s", body)
	}
	// The DB's own NOT NULL violation used to surface here. Its message is
	// "missing required field", which describes a different mistake.
	if strings.Contains(body, "missing required field") {
		t.Errorf("this must be refused in validation, not by the database: %s", body)
	}
}

// The headline: both write sources must answer identically, and refuse.
func TestNullWrite_RefusedIdenticallyOnEitherPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapPath bool
	}{
		{"record_sourced", false},
		{"map_sourced", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := nwServer(t, tc.mapPath)
			id := srv.MustID(srv.POST("/nw_docs", map[string]any{
				"name": "doc", "note": "something",
			}))

			assertNullRefused(t, srv.PATCH("/nw_docs/"+id,
				map[string]any{"note": nil}), "note")

			// And the stored value is untouched by the refused write.
			if got := srv.GET("/nw_docs/" + id).Data()["note"]; got != "something" {
				t.Errorf("a refused write must not change the row, note=%v", got)
			}
		})
	}
}

// Create takes the same rule.
func TestNullWrite_RefusedOnCreate(t *testing.T) {
	srv := nwServer(t, false)
	assertNullRefused(t, srv.POST("/nw_docs", map[string]any{
		"name": "doc", "note": nil,
	}), "note")
}

// A pointer field is exactly how you ask for a nullable column, so null is
// accepted there and genuinely stores NULL. Without this the fix could refuse
// every null and still pass everything above.
func TestNullWrite_PointerFieldAcceptsNull(t *testing.T) {
	srv := nwServer(t, false)
	id := srv.MustID(srv.POST("/nw_docs", map[string]any{
		"name": "doc", "note": "n", "ptr": "set",
	}))
	if nwIsNull(t, srv, "ptr", id) {
		t.Fatal("precondition: ptr should hold a value before the null write")
	}

	srv.PATCH("/nw_docs/"+id, map[string]any{"ptr": nil}).
		AssertStatus(http.StatusOK)

	if !nwIsNull(t, srv, "ptr", id) {
		t.Error("a null on a pointer field must store SQL NULL")
	}
}

// An omitted key is not a null — PATCH leaves an absent field alone.
func TestNullWrite_OmittedFieldIsUntouched(t *testing.T) {
	srv := nwServer(t, false)
	id := srv.MustID(srv.POST("/nw_docs", map[string]any{
		"name": "doc", "note": "keep me",
	}))

	srv.PATCH("/nw_docs/"+id, map[string]any{"name": "renamed"}).
		AssertStatus(http.StatusOK)

	if got := srv.GET("/nw_docs/" + id).Data()["note"]; got != "keep me" {
		t.Errorf("an omitted field must be left alone, got note=%v", got)
	}
}

// An empty string is a value, not a null, and must still be accepted.
func TestNullWrite_EmptyStringIsAccepted(t *testing.T) {
	srv := nwServer(t, false)
	id := srv.MustID(srv.POST("/nw_docs", map[string]any{
		"name": "doc", "note": "something",
	}))

	srv.PATCH("/nw_docs/"+id, map[string]any{"note": ""}).
		AssertStatus(http.StatusOK)

	if nwIsNull(t, srv, "note", id) {
		t.Error("an explicit empty string must store \"\", not NULL")
	}
	if got := srv.GET("/nw_docs/" + id).Data()["note"]; got != "" {
		t.Errorf("note should be the empty string, got %v", got)
	}
}
