package e2e

// Audit PIPE-5 — AtPosition(Replace) swaps out a step's whole default handler,
// and the Validate default was the only thing stripping what a client may not
// write: the generated id, mfx:"readonly", and mfx:"immutable" on update. A
// one-line "custom validator" therefore turned all three off, and nothing said
// so — a client could set role, owner_id, created_at, or the tenant column
// db.Tenancy stamps.
//
// The strip now also runs in a fixed segment after the Validate step, which a
// Replace cannot remove. It is idempotent, so the default path is unchanged.
//
// The Response half cannot be closed the same way: hidden, write-only and
// encrypted fields are dropped by the default serializer, and a Replace that
// serializes ctx.DBResult itself is past it. That one warns at boot instead —
// see TestReplaceResponse_WarnsWhenItTakesOverARedactedModel.

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// passThroughValidator is what "replace validation with my own" looks like: it
// checks whatever the application cares about and passes the request on.
func passThroughValidator(ctx *maniflex.ServerContext, next func() error) error {
	return next()
}

func replacedValidateSrv(t *testing.T, extra func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Validate.Register(passThroughValidator, maniflex.AtPosition(maniflex.Replace))
			if extra != nil {
				extra(s)
			}
		},
	})
}

func rgUser(t *testing.T, srv *testutil.Server, email string) string {
	t.Helper()
	return srv.MustID(srv.POST("/users", map[string]any{
		"name": "Ada", "email": email, "password": "pw"}))
}

// The three tags the Validate default was the only enforcer of.
func TestReplaceValidate_ClientStillCannotWriteProtectedFields(t *testing.T) {
	t.Parallel()
	srv := replacedValidateSrv(t, nil)
	uid := rgUser(t, srv, "protected@x.com")

	t.Run("generated_id", func(t *testing.T) {
		data := srv.POST("/posts", map[string]any{
			"id": "client-chosen-id", "title": "T", "body": "B",
			"status": "draft", "user_id": uid}).
			AssertStatus(http.StatusCreated).Data()
		if data["id"] == "client-chosen-id" {
			t.Error("the client chose the row's id; only the adapter may")
		}
	})

	t.Run("readonly_on_create", func(t *testing.T) {
		data := srv.POST("/posts", map[string]any{
			"title": "T", "body": "B", "status": "draft",
			"user_id": uid, "views": 999}).
			AssertStatus(http.StatusCreated).Data()
		if got := data["views"]; got != float64(0) {
			t.Errorf("views = %#v, want 0 — mfx:\"readonly\" is not a client-writable field", got)
		}
	})

	t.Run("readonly_on_update", func(t *testing.T) {
		id := srv.MustID(srv.POST("/posts", map[string]any{
			"title": "T", "body": "B", "status": "draft", "user_id": uid}))
		data := srv.PATCH("/posts/"+id, map[string]any{"views": 999}).
			AssertStatus(http.StatusOK).Data()
		if got := data["views"]; got != float64(0) {
			t.Errorf("views = %#v after a PATCH, want 0", got)
		}
	})

	t.Run("immutable_on_update", func(t *testing.T) {
		id := rgUser(t, srv, "immutable@x.com")
		data := srv.PATCH("/users/"+id, map[string]any{"email": "changed@x.com"}).
			AssertStatus(http.StatusOK).Data()
		if got := data["email"]; got != "immutable@x.com" {
			t.Errorf("email = %#v, want the original — mfx:\"immutable\" refuses an update", got)
		}
	})
}

// The escape that must survive: a value the *server* stamped is not from a
// client. Without this, a scope or auth middleware that fills a readonly column
// would have its injection stripped and the row written empty — invisible to the
// tenant that created it.
func TestReplaceValidate_ServerStampedValueStillLands(t *testing.T) {
	t.Parallel()
	srv := replacedValidateSrv(t, func(s *maniflex.Server) {
		s.Pipeline.Validate.Register(
			func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("views", 42)
				return next()
			},
			maniflex.ForModel("Post"),
			maniflex.AtPosition(maniflex.Before),
		)
	})
	uid := rgUser(t, srv, "stamped@x.com")

	data := srv.POST("/posts", map[string]any{
		"title": "T", "body": "B", "status": "draft", "user_id": uid, "views": 999}).
		AssertStatus(http.StatusCreated).Data()
	if got := data["views"]; got != float64(42) {
		t.Errorf("views = %#v, want 42 — the server's own stamp must not be stripped", got)
	}
}

// Replace still means replace: the default's *validation* is gone, and only the
// strip is held back. A required field the custom validator does not check is
// the application's business, not something the segment quietly re-adds.
func TestReplaceValidate_CustomValidatorStillOwnsValidation(t *testing.T) {
	t.Parallel()
	srv := replacedValidateSrv(t, nil)

	// enum:draft|published|archived — the default Validate step would refuse
	// this; the replacement does not check it, so it is accepted.
	uid := rgUser(t, srv, "enum@x.com")
	srv.POST("/posts", map[string]any{
		"title": "T", "body": "B", "status": "not-a-real-status", "user_id": uid}).
		AssertStatus(http.StatusCreated)
}

// The strip runs after the Validate step, so middleware registered around it
// sees the body it has always seen.
func TestReplaceValidate_BeforeMiddlewareStillSeesTheClientValue(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen any
	srv := replacedValidateSrv(t, func(s *maniflex.Server) {
		s.Pipeline.Validate.Register(
			func(ctx *maniflex.ServerContext, next func() error) error {
				v, _ := ctx.Field("views")
				mu.Lock()
				seen = v
				mu.Unlock()
				return next()
			},
			maniflex.ForModel("Post"),
			maniflex.AtPosition(maniflex.Before),
		)
	})
	uid := rgUser(t, srv, "before@x.com")

	srv.POST("/posts", map[string]any{
		"title": "T", "body": "B", "status": "draft", "user_id": uid, "views": 999}).
		AssertStatus(http.StatusCreated)

	mu.Lock()
	defer mu.Unlock()
	if seen != float64(999) {
		t.Errorf("a Before-Validate middleware saw views = %#v, want the client's 999 — "+
			"the strip must not move earlier than it was", seen)
	}
}

// The default path is what almost every request takes; the refactor that made
// the strip reusable must not have changed it.
func TestValidate_DefaultPathUnchanged(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{})
	uid := rgUser(t, srv, "default@x.com")

	data := srv.POST("/posts", map[string]any{
		"title": "T", "body": "B", "status": "draft", "user_id": uid, "views": 999}).
		AssertStatus(http.StatusCreated).Data()
	if got := data["views"]; got != float64(0) {
		t.Errorf("views = %#v on the default path, want 0", got)
	}
	// And the default still validates, which is the half Replace gives up.
	srv.POST("/posts", map[string]any{
		"title": "T", "body": "B", "status": "not-a-real-status", "user_id": uid}).
		AssertStatus(http.StatusUnprocessableEntity)
}

// ── Response ─────────────────────────────────────────────────────────────────

// lockedBuf is a bytes.Buffer safe for concurrent writes from the logger.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A Replace on the Response step is past the only thing that drops hidden,
// write-only and encrypted fields, and the framework cannot see whether the
// replacement drops them itself. So it says so at boot rather than refusing a
// correct one or staying silent about a leaking one.
func TestReplaceResponse_WarnsWhenItTakesOverARedactedModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		models   []any
		position maniflex.Position
		wantWarn bool
	}{
		{"replace over a model with hidden fields", []any{testutil.ExportableRow{}}, maniflex.Replace, true},
		{"replace over a model with none", []any{testutil.WorkflowDoc{}}, maniflex.Replace, false},
		{"after leaves the default response in place", []any{testutil.ExportableRow{}}, maniflex.After, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf lockedBuf
			testutil.NewServer(t, testutil.Options{
				Models: tc.models,
				Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.Response.Register(
						func(ctx *maniflex.ServerContext, next func() error) error { return next() },
						maniflex.AtPosition(tc.position))
				},
			})

			warned := strings.Contains(buf.String(), "takes over the Response step")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log: %s", warned, tc.wantWarn, buf.String())
			}
			if tc.wantWarn {
				for _, want := range []string{"ExportableRow", "secret", "notes", "RedactRecord"} {
					if !strings.Contains(buf.String(), want) {
						t.Errorf("the warning must name %q: %s", want, buf.String())
					}
				}
			}
		})
	}
}
