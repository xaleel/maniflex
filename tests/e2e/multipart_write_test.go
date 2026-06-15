package e2e

// P1 #8 — multipart/form-data writes. The typed migration deliberately leaves
// multipart bodies ParsedBody-authoritative (form values don't struct-decode),
// so the fallback write path must still persist plain scalar form fields. This
// model has no file fields, so it exercises the pure-scalar multipart path.

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

type mpDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
	Note string `json:"note" db:"note"`
}

func TestMultipart_ScalarFieldsPersist(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{mpDoc{}}})

	resp := srv.POSTMultipart("/mp_docs", map[string]string{
		"name": "viaform",
		"note": "hello from multipart",
	}, nil)
	resp.AssertStatus(http.StatusCreated)

	got := srv.GET("/mp_docs/" + resp.ID()).Data()
	if v := testutil.Field(t, got, "name"); v != "viaform" {
		t.Errorf("name = %q, want viaform", v)
	}
	if v := testutil.Field(t, got, "note"); v != "hello from multipart" {
		t.Errorf("note = %q, want 'hello from multipart'", v)
	}

	// PATCH via multipart updates a scalar too.
	srv.PATCHMultipart("/mp_docs/"+resp.ID(), map[string]string{"note": "edited"}, nil).
		AssertStatus(http.StatusOK)
	if v := testutil.Field(t, srv.GET("/mp_docs/"+resp.ID()).Data(), "note"); v != "edited" {
		t.Errorf("after multipart PATCH: note = %q, want edited", v)
	}
}
