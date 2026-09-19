package e2e

// Audit AUTH-2: RequireOwner and Enforce dispatch on ctx.Operation and handled
// only the CRUD operations, so the two reads derived from OpRead — a record's
// history and its attachment downloads — fell through to next() unchecked.
//
// They fell through precisely because they are covered: ForOperation(OpRead)
// aliases to OpReadHistory and OpReadAttachment (middleware.go), so both
// middlewares were invoked on those routes and then matched no case. A non-owner
// read another user's field-level diffs and file bytes at endpoints whose own
// documentation calls them scoped.
//
//	go test ./tests/e2e/... -run TestDerivedRead

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type derivedNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title" mfx:"searchable"`
	OwnerID string `json:"owner_id" db:"owner_id"`
	File    string `json:"file" db:"file" mfx:"file"`
}

// derivedServer mounts derivedNote with history, attachments and export, behind
// a header-driven test identity, and lets each test add its own guard.
func derivedServer(t *testing.T, register func(s *maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{derivedNote{}, maniflex.ModelConfig{
			Versioned:     true,
			ExportEnabled: true,
		}},
		FileStorage: testutil.NewMemoryStorage(),
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				uid := ctx.Request.Header.Get("X-Test-User")
				if uid == "" {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
					return nil
				}
				info := &maniflex.AuthInfo{UserID: uid}
				if r := ctx.Request.Header.Get("X-Test-Role"); r != "" {
					info.Roles = []string{r}
				}
				ctx.Auth = info
				return next()
			})
			register(s)
		},
	})
}

// seedDerivedNote creates a note owned by alice, with an attachment, and updates
// it once so the history table actually holds a diff to leak.
func seedDerivedNote(t *testing.T, srv *testutil.Server) string {
	t.Helper()
	created := srv.POSTMultipart("/derived_notes",
		map[string]string{"title": "secret plans", "owner_id": "alice"},
		map[string]testutil.FileUpload{
			"file": {Filename: "r.pdf", ContentType: "application/pdf", Body: fakePDF},
		}, asUser("alice"))
	created.AssertStatus(http.StatusCreated)
	id := created.ID()
	srv.PATCH("/derived_notes/"+id, map[string]any{"title": "revised plans"}, asUser("alice")).
		AssertStatus(http.StatusOK)
	return id
}

// assertOwnerStillReaches is the must-still-work half of every case below. A
// guard that denied everyone would satisfy the denial assertions perfectly.
func assertOwnerStillReaches(t *testing.T, srv *testutil.Server, id string, who map[string]string, label string) {
	t.Helper()
	hist := srv.GET("/derived_notes/"+id+"/history", who)
	hist.AssertStatus(http.StatusOK)
	if !strings.Contains(string(hist.Body), "secret plans") {
		t.Errorf("%s: history did not carry the recorded diff:\n%s", label, hist.Body)
	}
	file := srv.GETRawWithHeaders("/derived_notes/"+id+"/file", who)
	file.AssertStatus(http.StatusOK)
	if string(file.Body) != string(fakePDF) {
		t.Errorf("%s: attachment bytes differ from what was uploaded", label)
	}
}

func TestDerivedRead_RequireOwnerScopesHistoryAndAttachment(t *testing.T) {
	t.Parallel()
	srv := derivedServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := seedDerivedNote(t, srv)
	bob := asUser("bob")

	// The direct read is the baseline: bob is already refused there, and 404
	// rather than 403 so the endpoint does not admit the record exists.
	srv.GET("/derived_notes/"+id, bob).AssertStatus(http.StatusNotFound)

	// The same record in its two other read shapes must answer the same way.
	srv.GET("/derived_notes/"+id+"/history", bob).AssertStatus(http.StatusNotFound)
	srv.GETRawWithHeaders("/derived_notes/"+id+"/file", bob).AssertStatus(http.StatusNotFound)

	assertOwnerStillReaches(t, srv, id, asUser("alice"), "owner")
}

// adminRoles bypass ownership on reads, and must go on bypassing it on the two
// reads that now run the check.
func TestDerivedRead_RequireOwnerAdminBypassSurvives(t *testing.T) {
	t.Parallel()
	srv := derivedServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", "admin"))
	})
	id := seedDerivedNote(t, srv)

	assertOwnerStillReaches(t, srv, id, asUser("root", "admin"), "admin")
}

func TestDerivedRead_EnforceScopesHistoryAndAttachment(t *testing.T) {
	t.Parallel()
	srv := derivedServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
			if ctx.Auth == nil {
				return false, nil
			}
			owner, _ := res["owner_id"].(string)
			return owner == ctx.Auth.UserID, nil
		}))
	})
	id := seedDerivedNote(t, srv)
	bob := asUser("bob")

	srv.GET("/derived_notes/"+id, bob).AssertStatus(http.StatusForbidden)
	srv.GET("/derived_notes/"+id+"/history", bob).AssertStatus(http.StatusForbidden)
	srv.GETRawWithHeaders("/derived_notes/"+id+"/file", bob).AssertStatus(http.StatusForbidden)

	assertOwnerStillReaches(t, srv, id, asUser("alice"), "owner")
}

// Export streams bytes and leaves ctx.Response nil, so enforceAfterList has
// nothing to filter — and ForOperation(OpList) covers OpExport by alias, so a
// policy that filtered the list streamed the whole table at /export. Enforce
// refuses it rather than passing it.
func TestDerivedRead_EnforceRefusesExportItCannotFilter(t *testing.T) {
	t.Parallel()
	srv := derivedServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
			if ctx.Auth == nil {
				return false, nil
			}
			owner, _ := res["owner_id"].(string)
			return owner == ctx.Auth.UserID, nil
		}))
	})
	seedDerivedNote(t, srv)

	exp := srv.GET("/derived_notes/export?format=csv", asUser("bob"))
	exp.AssertStatus(http.StatusForbidden)
	// The message has to send the reader somewhere that works, or refusing the
	// route is just a dead end.
	if !strings.Contains(string(exp.Body), "db.ForceFilter") {
		t.Errorf("refusal should name the middleware that can scope an export:\n%s", exp.Body)
	}
	// Refused for the owner too: the policy cannot be evaluated either way, and
	// an export that worked for whoever happened to own row one would be worse.
	srv.GET("/derived_notes/export?format=csv", asUser("alice")).AssertStatus(http.StatusForbidden)

	// Must still work: the list the export is derived from is filtered, not
	// refused. Without this, blanket-403ing everything would pass the above.
	testutil.AssertLen(t, "owner list", srv.GET("/derived_notes", asUser("alice")).DataList(), 1)
	testutil.AssertLen(t, "stranger list", srv.GET("/derived_notes", asUser("bob")).DataList(), 0)
}

// Enforce is documented for the DB step, and OpSearch skips the DB step
// entirely — so a DB-registered Enforce never sees /search and cannot scope it.
// Registered on Auth it does see it, and refuses for the same reason as export.
// Pinned because the difference is invisible at the call site: the same Enforce
// call protects or ignores the endpoint depending only on which step it is on.
func TestDerivedRead_EnforceReachesSearchOnlyFromTheAuthStep(t *testing.T) {
	t.Parallel()
	deny := func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) { return false, nil }

	for _, tc := range []struct {
		name       string
		register   func(s *maniflex.Server)
		wantStatus int
	}{
		{"DB step: never reached", func(s *maniflex.Server) {
			s.Pipeline.DB.Register(auth.Enforce(deny))
		}, http.StatusOK},
		{"Auth step: refused", func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.Enforce(deny))
		}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := derivedServer(t, func(s *maniflex.Server) {
				s.EnableGlobalSearch(maniflex.GlobalSearchConfig{MaxLimit: 10})
				tc.register(s)
			})
			srv.GET("/search?q=plans", asUser("bob")).AssertStatus(tc.wantStatus)
		})
	}
}

// The operations Enforce cannot decide about must keep passing. Failing the
// default branch closed would 403 CORS preflight, which is why the refusal above
// is limited to the two operations that actually return rows.
func TestDerivedRead_EnforcePassesOperationsItCannotDecide(t *testing.T) {
	t.Parallel()
	srv := derivedServer(t, func(s *maniflex.Server) {
		s.Pipeline.DB.Register(auth.Enforce(func(*maniflex.ServerContext, map[string]any) (bool, error) {
			return false, nil
		}))
	})
	// No record needed, and none could be seeded anyway: this policy denies
	// everything it is asked about, which is the point.
	rec := srv.Do(http.MethodOptions,
		srv.APIPath("/derived_notes/00000000-0000-0000-0000-000000000000"), nil, asUser("bob"))
	if rec.Status == http.StatusForbidden {
		t.Errorf("Enforce 403'd an OPTIONS preflight; the default branch must pass it")
	}
}
