package e2e

// MS-1 — a bare mfx:"hidden" field was excluded from responses but still
// accepted from a request body, so `IsAdmin bool` tagged hidden could be set by
// anyone willing to guess the column name — and because the field is scrubbed
// from every response, nothing revealed that it had happened. The docs and the
// generated OpenAPI create/update schemas both already said hidden fields cannot
// be written; only the write path disagreed.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type hiddenAccount struct {
	maniflex.BaseModel
	Name     string `json:"name"`
	IsAdmin  bool   `json:"is_admin"  mfx:"hidden"`    // server-owned; client must not set it
	Password string `json:"password"  mfx:"writeonly"` // client sets it, never reads it back
}

// hiddenSrv returns the server plus a background context for reading past the
// response scrubber — a hidden field never comes back over HTTP, so asserting on
// the response body could not tell a stripped write from a stored one.
func hiddenSrv(t *testing.T) (*testutil.Server, *maniflex.ServerContext) {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{hiddenAccount{}}})
	mfx := srv.ManiflexServer()
	return srv, maniflex.NewBackground(t.Context(), mfx.DB(), mfx.Registry())
}

func TestHidden_NotSettableOnCreate(t *testing.T) {
	srv, bg := hiddenSrv(t)

	resp := srv.POST("/hidden_accounts", map[string]any{
		"name":     "mallory",
		"is_admin": true,
		"password": "hunter2",
	})
	resp.AssertStatus(http.StatusCreated)

	rec, err := maniflex.Read[hiddenAccount](bg, resp.ID())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rec.IsAdmin {
		t.Error("a create body set the hidden is_admin column to true — mass assignment " +
			"into a field the client is not supposed to be able to write, and the response " +
			"scrubs the field so nothing reveals it")
	}
	if rec.Name != "mallory" {
		t.Errorf("name = %q, want %q — the strip took an ordinary field with it", rec.Name, "mallory")
	}
}

func TestHidden_NotSettableOnUpdate(t *testing.T) {
	srv, bg := hiddenSrv(t)

	id := srv.POST("/hidden_accounts", map[string]any{
		"name": "mallory", "password": "hunter2",
	}).AssertStatus(http.StatusCreated).ID()

	// The PATCH is accepted — a stripped field is silently dropped, the same way
	// mfx:"readonly" and a client-supplied id already are.
	srv.PATCH("/hidden_accounts/"+id, map[string]any{"is_admin": true}).
		AssertStatus(http.StatusOK)

	rec, err := maniflex.Read[hiddenAccount](bg, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rec.IsAdmin {
		t.Error("PATCH {\"is_admin\":true} escalated a hidden field — privilege escalation " +
			"via mass assignment")
	}
}

// The counterpart that must keep working: writeonly is the deliberate
// "client writes it, never reads it back" case. If hidden's new implication
// leaked onto it, every password field in the wild would silently stop being
// stored — a far worse failure than the one being fixed.
func TestWriteOnly_StillAcceptedFromClient(t *testing.T) {
	srv, bg := hiddenSrv(t)

	resp := srv.POST("/hidden_accounts", map[string]any{
		"name": "alice", "password": "hunter2",
	})
	resp.AssertStatus(http.StatusCreated)

	rec, err := maniflex.Read[hiddenAccount](bg, resp.ID())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rec.Password != "hunter2" {
		t.Errorf("stored password = %q, want %q — a writeonly field was stripped from the "+
			"write, so the client's value never landed", rec.Password, "hunter2")
	}

	// And it must still be scrubbed from the response.
	if _, ok := resp.Data()["password"]; ok {
		t.Error("writeonly password came back in the create response")
	}
	if _, ok := resp.Data()["is_admin"]; ok {
		t.Error("hidden is_admin came back in the create response")
	}
}

// A hidden field is still the server's to populate — the docs promise that a
// middleware inside the pipeline can set it. The Validate step strips the client's
// value, and Service runs after Validate, so a server-set value must survive.
func TestHidden_ServerSideMiddlewareCanStillSet(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{hiddenAccount{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("is_admin", true) // the server's own decision
				return next()
			}, maniflex.ForOperation(maniflex.OpCreate))
		},
	})
	mfx := srv.ManiflexServer()
	bg := maniflex.NewBackground(t.Context(), mfx.DB(), mfx.Registry())

	resp := srv.POST("/hidden_accounts", map[string]any{"name": "root", "password": "x"})
	resp.AssertStatus(http.StatusCreated)

	rec, err := maniflex.Read[hiddenAccount](bg, resp.ID())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !rec.IsAdmin {
		t.Error("a Service-step middleware could not set a hidden field — the readonly " +
			"implication is stripping the server's own writes, not just the client's")
	}
}
