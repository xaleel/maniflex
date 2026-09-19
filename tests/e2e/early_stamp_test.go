package e2e

// Audit AUTH-3: a value a middleware stamps with ctx.SetField before the body is
// parsed was overwritten by the body. auth.RequireOwner stamps the owner on the
// Auth step, Deserialize then copied the client's keys over it — and SetField
// had already marked the key server-set, which is what exempts a field from the
// readonly strip. So the client chose the owner, and a readonly owner column
// that held on its own was opened by adding RequireOwner to it.
//
//	go test ./tests/e2e/... -run TestEarlyStamp

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type stampPlainNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id"`
}

type stampReadonlyNote struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"readonly"`
}

// stampRole carries a readonly role that the tests' own Auth-step middleware
// stamps, to pin the rule for app middleware and not only for RequireOwner.
type stampRole struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name"`
	Role string `json:"role" db:"role" mfx:"readonly"`
}

// testIdentity installs the X-Test-User principal every test here authenticates as.
func testIdentity(s *maniflex.Server) {
	s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
		uid := ctx.Request.Header.Get("X-Test-User")
		if uid == "" {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return nil
		}
		ctx.Auth = &maniflex.AuthInfo{UserID: uid}
		return next()
	})
}

// storedOwner reports who owns the row as written, not as the create response
// echoed it: under RequireOwner only the stored owner can read the row back.
func storedOwner(t *testing.T, srv *testutil.Server, path string) string {
	t.Helper()
	for _, who := range []string{"alice", "bob"} {
		if r := srv.GET(path, asUser(who)); r.Status == http.StatusOK {
			return who
		}
	}
	t.Fatalf("%s is readable by neither alice nor bob", path)
	return ""
}

func TestEarlyStamp_ClientCannotChooseTheOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		model     any
		route     string
		fields    map[string]string
		multipart bool
	}{
		// Must still work: the stamp applies when the client says nothing.
		{"body omits the owner", stampPlainNote{}, "/stamp_plain_notes",
			map[string]string{"title": "t"}, false},
		{"plain column, JSON", stampPlainNote{}, "/stamp_plain_notes",
			map[string]string{"title": "t", "owner_id": "bob"}, false},
		{"readonly column, JSON", stampReadonlyNote{}, "/stamp_readonly_notes",
			map[string]string{"title": "t", "owner_id": "bob"}, false},
		{"plain column, multipart", stampPlainNote{}, "/stamp_plain_notes",
			map[string]string{"title": "t", "owner_id": "bob"}, true},
		{"readonly column, multipart", stampReadonlyNote{}, "/stamp_readonly_notes",
			map[string]string{"title": "t", "owner_id": "bob"}, true},
		// The typed record is decoded case-insensitively, so this key lands in
		// OwnerID there even though it is a different key in the parsed body.
		{"readonly column, case-folded key", stampReadonlyNote{}, "/stamp_readonly_notes",
			map[string]string{"title": "t", "OWNER_ID": "bob"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{tc.model},
				Middleware: func(s *maniflex.Server) {
					testIdentity(s)
					s.Pipeline.Auth.Register(auth.RequireOwner("owner_id"))
				},
			})

			var resp *testutil.Response
			if tc.multipart {
				resp = srv.POSTMultipart(tc.route, tc.fields, nil, asUser("alice"))
			} else {
				body := make(map[string]any, len(tc.fields))
				for k, v := range tc.fields {
					body[k] = v
				}
				resp = srv.POST(tc.route, body, asUser("alice"))
			}
			resp.AssertStatus(http.StatusCreated)
			if got := resp.Data()["owner_id"]; got != "alice" {
				t.Errorf("create answered owner_id %v, want alice", got)
			}
			if got := storedOwner(t, srv, tc.route+"/"+resp.ID()); got != "alice" {
				t.Errorf("row was written owned by %s, want alice", got)
			}
		})
	}
}

// Must still work: a readonly column with no stamp on it is stripped, exactly as
// before. This is the comparison that made the bug a regression rather than a
// gap — the tag held until RequireOwner was added.
func TestEarlyStamp_ReadonlyStillStripsWithoutAStamp(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models:     []any{stampReadonlyNote{}},
		Middleware: testIdentity,
	})
	resp := srv.POST("/stamp_readonly_notes",
		map[string]any{"title": "t", "owner_id": "bob"}, asUser("alice"))
	resp.AssertStatus(http.StatusCreated)
	if got := resp.Data()["owner_id"]; got != "" {
		t.Errorf("readonly owner_id with no stamp was written as %v, want it stripped", got)
	}
}

// The rule is SetField's, not RequireOwner's: an app's own Auth-step stamp is
// re-asserted the same way, on update as well as create.
func TestEarlyStamp_AppMiddlewareStampSurvivesTheBody(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{stampRole{}},
		Middleware: func(s *maniflex.Server) {
			testIdentity(s)
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Operation == maniflex.OpCreate || ctx.Operation == maniflex.OpUpdate {
					ctx.SetField("role", "member")
				}
				return next()
			})
		},
	})

	created := srv.POST("/stamp_roles", map[string]any{"name": "n", "role": "admin"}, asUser("alice"))
	created.AssertStatus(http.StatusCreated)
	if got := created.Data()["role"]; got != "member" {
		t.Fatalf("create: role %v, want the stamped member", got)
	}
	id := created.ID()

	updated := srv.PATCH("/stamp_roles/"+id, map[string]any{"role": "admin"}, asUser("alice"))
	updated.AssertStatus(http.StatusOK)
	if got := updated.Data()["role"]; got != "member" {
		t.Errorf("update: role %v, want the stamped member", got)
	}
}

// DeleteField un-marks the key. Left marked, the re-assert would write back a
// field a middleware had removed, and the client's value would still be exempt
// from the readonly strip — the same bug by another door.
func TestEarlyStamp_DeleteFieldWithdrawsTheStamp(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{stampRole{}},
		Middleware: func(s *maniflex.Server) {
			testIdentity(s)
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("role", "member")
				ctx.DeleteField("role") // changed its mind
				return next()
			})
		},
	})

	resp := srv.POST("/stamp_roles", map[string]any{"name": "n", "role": "admin"}, asUser("alice"))
	resp.AssertStatus(http.StatusCreated)
	switch got := resp.Data()["role"]; got {
	case "":
		// Withdrawn, and the client's value stripped as readonly.
	case "member":
		t.Error("the withdrawn stamp was written back")
	default:
		t.Errorf("role %v: the client's value kept the server-set exemption after DeleteField", got)
	}
}

// Scope providers run after the re-assert, so one that reads the field sees the
// stamp rather than the value the client tried to replace it with.
func TestEarlyStamp_ScopeProvidersSeeTheStamp(t *testing.T) {
	t.Parallel()
	var seen any
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{stampPlainNote{}},
		Middleware: func(s *maniflex.Server) {
			testIdentity(s)
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id"))
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Operation == maniflex.OpCreate {
					seen, _ = ctx.Field("owner_id")
				}
				return next()
			}, maniflex.ForModel("stampPlainNote"), maniflex.ProvidesScope())
		},
	})

	srv.POST("/stamp_plain_notes", map[string]any{"title": "t", "owner_id": "bob"}, asUser("alice")).
		AssertStatus(http.StatusCreated)
	if seen != "alice" {
		t.Errorf("scope provider saw owner_id %v, want the stamp", seen)
	}
}

// The re-assert is a fixed segment rather than part of the default Deserialize
// handler, so replacing that handler does not bring the bug back. A replacement
// that parses the body its own way overwrites the stamp exactly as the default
// did.
func TestEarlyStamp_SurvivesAReplacedDeserializeStep(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{stampPlainNote{}},
		Middleware: func(s *maniflex.Server) {
			testIdentity(s)
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id"))
			s.Pipeline.Deserialize.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				raw, err := io.ReadAll(ctx.Request.Body)
				if err != nil {
					return err
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					return err
				}
				// A fresh body, as a hand-written parser naturally builds one: the
				// stamp is not overwritten here so much as thrown away.
				ctx.ParsedBody = maniflex.NewRequestBody(m)
				return next()
				// Creates only: the default handler also builds ctx.Query, which the
				// read-back below needs.
			}, maniflex.ForModel("stampPlainNote"), maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.Replace))
		},
	})

	resp := srv.POST("/stamp_plain_notes", map[string]any{"title": "t", "owner_id": "bob"}, asUser("alice"))
	resp.AssertStatus(http.StatusCreated)
	if got := storedOwner(t, srv, "/stamp_plain_notes/"+resp.ID()); got != "alice" {
		t.Errorf("with Deserialize replaced, the row was written owned by %s, want alice", got)
	}
}
