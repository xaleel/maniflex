package e2e

// Audit STEP-3 — syncRecordField returned early when the value was nil, leaving
// the typed record's field holding whatever bindRecord had bound from the
// client's body while its present-flag stayed set. recordSourcedWrite then
// emitted the client's value for the very key the server had just cleared.
//
// The shape that matters is a middleware clearing a client-supplied field:
// ctx.SetField("owner_id", nil) believed it was writing NULL and stored the id
// the client sent instead. The map fallback would have written NULL, so whether
// the clear worked depended on which write path the request happened to take.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ClearedDoc struct {
	maniflex.BaseModel
	Title   string  `json:"title"    db:"title"    mfx:"required"`
	OwnerID *string `json:"owner_id" db:"owner_id" mfx:"filterable"`
}

// clearingSrv strips owner_id the way a middleware assigning ownership from the
// authenticated caller would: whatever the client sent is not to be trusted.
func clearingSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{ClearedDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("owner_id", nil)
				return next()
			}, maniflex.ForModel("ClearedDoc"))
		},
	})
}

func TestSetFieldNil_ClearsAClientSuppliedFieldOnCreate(t *testing.T) {
	t.Parallel()
	srv := clearingSrv(t)

	resp := srv.POST("/cleared_docs", map[string]any{
		"title":    "t",
		"owner_id": "someone-elses-id",
	})
	resp.AssertStatus(http.StatusCreated)

	if got := resp.Data()["owner_id"]; got != nil {
		t.Errorf("create stored owner_id = %#v; the middleware cleared it, so the "+
			"client's value must not survive", got)
	}
	// Read it back: the response could be right while the row is wrong.
	if got := srv.GET("/cleared_docs/" + resp.ID()).Data()["owner_id"]; got != nil {
		t.Errorf("the stored row has owner_id = %#v, want null", got)
	}
}

func TestSetFieldNil_ClearsAClientSuppliedFieldOnUpdate(t *testing.T) {
	t.Parallel()
	srv := clearingSrv(t)

	id := srv.MustID(srv.POST("/cleared_docs", map[string]any{"title": "t"}))

	srv.PATCH("/cleared_docs/"+id, map[string]any{"owner_id": "someone-elses-id"}).
		AssertStatus(http.StatusOK)

	if got := srv.GET("/cleared_docs/" + id).Data()["owner_id"]; got != nil {
		t.Errorf("after a PATCH the row has owner_id = %#v, want null", got)
	}
}

// The clear must not be a blanket wipe: a value the middleware does not touch
// still round-trips, so the fix cannot be "nil everything".
func TestSetFieldNil_LeavesOtherFieldsAlone(t *testing.T) {
	t.Parallel()
	srv := clearingSrv(t)

	resp := srv.POST("/cleared_docs", map[string]any{
		"title":    "kept",
		"owner_id": "someone-elses-id",
	})
	resp.AssertStatus(http.StatusCreated)
	if got := resp.Data()["title"]; got != "kept" {
		t.Errorf("title = %#v, want %q", got, "kept")
	}
}

// And a middleware that assigns rather than clears still wins over the client.
func TestSetFieldNil_AssignedValueStillWins(t *testing.T) {
	t.Parallel()
	owner := "the-authenticated-user"
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{ClearedDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("owner_id", &owner)
				return next()
			}, maniflex.ForModel("ClearedDoc"))
		},
	})

	resp := srv.POST("/cleared_docs", map[string]any{
		"title":    "t",
		"owner_id": "someone-elses-id",
	})
	resp.AssertStatus(http.StatusCreated)
	if got := resp.Data()["owner_id"]; got != owner {
		t.Errorf("owner_id = %#v, want %q", got, owner)
	}
}
