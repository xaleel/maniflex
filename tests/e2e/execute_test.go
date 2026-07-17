package e2e

// R3 — Server.Execute runs a registered model's operation through the full
// pipeline from Go, with a typed principal and the caller's transaction.
//
// The two defects it exists to delete are structural, not careless:
//   - a principal passed as a header is one any client can send, so every gate
//     grows an internet-reachable bypass;
//   - N HTTP requests cannot be one transaction, so a staged write that fails at
//     item 3 leaves 1 and 2 committed and re-running re-applies them.
//
// So the tests that matter are: the principal is honoured and cannot be forged,
// the whole pipeline really runs (not a shortcut past Validate/Auth), and a
// failure mid-batch rolls the batch back.

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

const execSecret = "an-execute-test-secret-of-at-least-32-bytes"

func TestExecute_CreateReadUpdateDelete(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@exec.com", "admin"))

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model:     "Post",
		Operation: maniflex.OpCreate,
		Body: map[string]any{
			"title": "made in process", "body": "b", "status": "draft", "user_id": user,
		},
	})
	if err != nil {
		t.Fatalf("Execute create: %v", err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}
	rec, _ := res.Data.(map[string]any)
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %#v", res.Data)
	}

	// The row is real and reachable over HTTP: Execute is the same pipeline, not
	// a parallel one writing somewhere else.
	got := srv.GET("/posts/" + id).AssertStatus(http.StatusOK)
	if v := got.Data()["title"]; v != "made in process" {
		t.Errorf("title over HTTP = %v, want %q", v, "made in process")
	}

	read, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpRead, ID: id,
	})
	if err != nil {
		t.Fatalf("Execute read: %v", err)
	}
	if v := read.Data.(map[string]any)["title"]; v != "made in process" {
		t.Errorf("read title = %v", v)
	}

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpUpdate, ID: id,
		Body: map[string]any{"title": "renamed"},
	}); err != nil {
		t.Fatalf("Execute update: %v", err)
	}
	if v := srv.GET("/posts/" + id).Data()["title"]; v != "renamed" {
		t.Errorf("title after Execute update = %v, want renamed", v)
	}

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpDelete, ID: id,
	}); err != nil {
		t.Fatalf("Execute delete: %v", err)
	}
	srv.GET("/posts/" + id).AssertStatus(http.StatusNotFound)
}

// A PATCH must leave columns the body omits alone. This is why Execute
// synthesises a request and lets the real Deserialize step bind the body rather
// than re-deriving presence: a second implementation of ctx.present is a second
// chance to corrupt an update.
func TestExecute_UpdateIsAPatchNotAReplace(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@patch.com", "admin"))
	id := srv.MustID(srv.CreatePost("original", "published", user))

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpUpdate, ID: id,
		Body: map[string]any{"title": "new title"}, // status deliberately absent
	}); err != nil {
		t.Fatalf("Execute update: %v", err)
	}

	got := srv.GET("/posts/" + id).Data()
	if got["title"] != "new title" {
		t.Errorf("title = %v, want %q", got["title"], "new title")
	}
	if got["status"] != "published" {
		t.Errorf("status = %v, want published — the omitted field was overwritten, so "+
			"Execute's update is a replace rather than a patch", got["status"])
	}
}

func TestExecute_ListHonoursQuery(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@list.com", "admin"))
	srv.MustID(srv.CreatePost("a", "published", user))
	srv.MustID(srv.CreatePost("b", "draft", user))

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList,
		Query: url.Values{"filter": {"status:eq:draft"}},
	})
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	// Execute returns the same envelope HTTP does: Data is the marshalled items
	// and Meta carries the pagination.
	items, ok := res.Data.([]any)
	if !ok {
		t.Fatalf("list Data is %T, want []any", res.Data)
	}
	if len(items) != 1 {
		t.Fatalf("list returned %d items, want 1 — ?filter= did not reach ParseQueryParams", len(items))
	}
	if got := items[0].(map[string]any)["title"]; got != "b" {
		t.Errorf("filtered item = %v, want b", got)
	}
	if res.Meta == nil || res.Meta.Total != 1 {
		t.Errorf("meta = %+v, want Total 1", res.Meta)
	}
}

// The headline: a typed principal, honoured by the app's own auth, with no
// header anywhere. Before this, the only way in was to forge one.
func TestExecute_TypedPrincipalPassesJWTAuth(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
		},
	})

	// A real client with no token is refused, so the gate is genuinely armed.
	srv.GET("/posts").AssertStatus(http.StatusUnauthorized)

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList,
		Auth: &maniflex.AuthInfo{UserID: "approver-1", Roles: []string{"admin"}},
	})
	if err != nil {
		t.Fatalf("Execute with a typed principal was refused by JWTAuth: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
}

// The principal must reach the steps, not merely get past the gate — a scope
// keyed on ctx.Auth is the whole reason Auth is not skippable.
func TestExecute_PrincipalReachesThePipeline(t *testing.T) {
	var seen *maniflex.AuthInfo
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				seen = ctx.Auth
				return next()
			})
		},
	})

	want := &maniflex.AuthInfo{UserID: "approver-1", Roles: []string{"admin"}}
	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList, Auth: want,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen == nil || seen.UserID != "approver-1" {
		t.Fatalf("DB step saw Auth = %#v, want UserID approver-1 — JWTAuth overwrote the "+
			"injected principal instead of standing aside", seen)
	}
}

// An Execute with no principal is anonymous and must still be refused. Being
// internal is not being authenticated: waving it through would make Execute a
// way to skip auth, which is the vulnerability, not the fix.
func TestExecute_NoPrincipalIsStillRefused(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
		},
	})

	_, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList, // Auth deliberately nil
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401 ExecuteError — an anonymous in-process call was "+
			"waved through because it was internal", err)
	}
}

// A client must not be able to reach the in-process gate. InProcess is derived
// from an unexported field, so this is a compile-time guarantee, but the header
// name is the one thing an attacker would try.
func TestExecute_ClientCannotForgeInProcess(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
		},
	})

	for _, h := range []map[string]string{
		{"X-In-Process": "1"},
		{"X-Execute": "true"},
		{"In-Process": "1"},
	} {
		srv.GET("/posts", h).AssertStatus(http.StatusUnauthorized)
	}
}

// The InProcess half of the gate, pinned. Over HTTP, JWTAuth OVERWRITES a
// principal an earlier Auth middleware set — so an app whose earlier middleware
// trusts a header is today saved by that overwrite. Had the gate keyed on
// ctx.Auth != nil alone, this request would sail through on a forged header:
// exactly the bypass class R3 exists to delete, reintroduced by the fix for it.
func TestExecute_HTTPPresetAuthDoesNotBypassJWT(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			// A naive app middleware that trusts a client header.
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if u := ctx.Request.Header.Get("X-User-Id"); u != "" {
					ctx.Auth = &maniflex.AuthInfo{UserID: u, Roles: []string{"admin"}}
				}
				return next()
			})
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
		},
	})

	srv.GET("/posts", map[string]string{"X-User-Id": "victim"}).
		AssertStatus(http.StatusUnauthorized)
}

// InProcess must be false for every real request, or transport middleware that
// consults it would stand aside for clients too.
func TestExecute_InProcessIsFalseOverHTTP(t *testing.T) {
	var overHTTP, viaExecute bool
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.InProcess() {
					viaExecute = true
				} else {
					overHTTP = true
				}
				return next()
			})
		},
	})

	srv.GET("/posts").AssertStatus(http.StatusOK)
	if !overHTTP || viaExecute {
		t.Fatalf("over HTTP: InProcess() = %v, want false", viaExecute)
	}

	overHTTP, viaExecute = false, false
	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !viaExecute || overHTTP {
		t.Fatalf("via Execute: InProcess() = %v, want true", viaExecute)
	}
}

// Validate still runs. Execute is the full pipeline, so a body that HTTP would
// reject must be rejected here identically — otherwise Execute is a way around
// the model's own rules.
func TestExecute_ValidateStillRuns(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@val.com", "admin"))

	_, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "t", "body": "b", "status": "bogus", "user_id": user},
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("err = %v, want a 422 ExecuteError — an invalid enum was accepted, so "+
			"Execute skipped Validate", err)
	}
}

// A readonly field is still readonly. The Validate step strips it; Execute must
// not become a mass-assignment hole.
func TestExecute_ReadonlyStillStripped(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@ro.com", "admin"))

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpCreate,
		Body: map[string]any{
			"title": "t", "body": "b", "status": "draft", "user_id": user,
			"views": 9999, // mfx:"readonly"
		},
	})
	if err != nil {
		t.Fatalf("Execute create: %v", err)
	}
	if v := res.Data.(map[string]any)["views"]; v == float64(9999) || v == 9999 {
		t.Errorf("views = %v — a readonly field written through Execute", v)
	}
}

// The other headline: N invocations in one transaction, rolled back together.
// Over HTTP this is impossible — N requests are N transactions — and the bug it
// produced was a committed prefix that re-applied on retry.
func TestExecute_TxRollsBackTheWholeBatch(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@tx.com", "admin"))
	a := srv.MustID(srv.CreatePost("first", "draft", user))
	b := srv.MustID(srv.CreatePost("second", "draft", user))

	bg := maniflex.NewBackground(t.Context(), srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
	tx, err := bg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}

	batch := []maniflex.Invocation{
		{Model: "Post", Operation: maniflex.OpUpdate, ID: a, Tx: tx,
			Body: map[string]any{"status": "published"}},
		{Model: "Post", Operation: maniflex.OpUpdate, ID: b, Tx: tx,
			Body: map[string]any{"status": "published"}},
		// Item 3 fails validation. The natural loop must abandon the batch.
		{Model: "Post", Operation: maniflex.OpUpdate, ID: a, Tx: tx,
			Body: map[string]any{"status": "not-a-status"}},
	}

	var failed bool
	for _, inv := range batch {
		if _, err := srv.ManiflexServer().Execute(t.Context(), inv); err != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("the batch did not fail — item 3 carries an invalid enum")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Neither of the first two may survive: that committed prefix is the bug.
	for _, id := range []string{a, b} {
		if got := srv.GET("/posts/" + id).Data()["status"]; got != "draft" {
			t.Errorf("post %s status = %v, want draft — a failed batch committed its "+
				"prefix, so re-running it would re-apply those writes", id, got)
		}
	}
}

// The same batch, committed: the transaction must actually carry the writes.
func TestExecute_TxCommitsTheWholeBatch(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})
	user := srv.MustID(srv.CreateUser("Ann", "ann@tx2.com", "admin"))
	a := srv.MustID(srv.CreatePost("first", "draft", user))
	b := srv.MustID(srv.CreatePost("second", "draft", user))

	bg := maniflex.NewBackground(t.Context(), srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
	tx, err := bg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for _, id := range []string{a, b} {
		if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
			Model: "Post", Operation: maniflex.OpUpdate, ID: id, Tx: tx,
			Body: map[string]any{"status": "published"},
		}); err != nil {
			t.Fatalf("Execute in tx: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for _, id := range []string{a, b} {
		if got := srv.GET("/posts/" + id).Data()["status"]; got != "published" {
			t.Errorf("post %s status = %v, want published", id, got)
		}
	}
}

// A missing record is an ExecuteError carrying the 404, not a silent nil.
func TestExecute_NotFoundIsAnError(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpRead,
		ID: "00000000-0000-0000-0000-000000000000",
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *ExecuteError", err)
	}
	if ee.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", ee.StatusCode)
	}
	if res == nil {
		t.Error("the response must still be returned alongside the error")
	}
}

// The refusals: streaming and trimmed-pipeline operations, and bad input.
func TestExecute_RefusedOperations(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{})

	for _, tc := range []struct {
		name string
		inv  maniflex.Invocation
	}{
		{"export streams bytes", maniflex.Invocation{Model: "Post", Operation: maniflex.OpExport}},
		{"attachment streams bytes", maniflex.Invocation{Model: "Post", Operation: maniflex.OpReadAttachment}},
		{"action has its own pipeline", maniflex.Invocation{Model: "Post", Operation: maniflex.OpAction}},
		{"search has its own pipeline", maniflex.Invocation{Model: "Post", Operation: maniflex.OpSearch}},
		{"unregistered model", maniflex.Invocation{Model: "Nope", Operation: maniflex.OpList}},
		{"no model", maniflex.Invocation{Operation: maniflex.OpList}},
		{"read without id", maniflex.Invocation{Model: "Post", Operation: maniflex.OpRead}},
		{"update without id", maniflex.Invocation{Model: "Post", Operation: maniflex.OpUpdate}},
		{"delete without id", maniflex.Invocation{Model: "Post", Operation: maniflex.OpDelete}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := srv.ManiflexServer().Execute(t.Context(), tc.inv); err == nil {
				t.Errorf("Execute(%+v) = nil error, want a refusal", tc.inv)
			}
		})
	}
}

// Execute must run the same middleware a request runs, in the same place — a
// Service-step hook is where an app's business logic lives, and an Execute that
// skipped it would be a second, quieter way into the database.
func TestExecute_ServiceMiddlewareStillRuns(t *testing.T) {
	var ran int
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ran++
				return next()
			}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate))
		},
	})
	user := srv.MustID(srv.CreateUser("Ann", "ann@svc.com", "admin"))

	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "t", "body": "b", "status": "draft", "user_id": user},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ran != 1 {
		t.Errorf("Service middleware ran %d times, want 1", ran)
	}
}

// Authorisation on the Auth step must bind an Execute exactly as it binds a
// request. This is DEC-E made concrete: Auth is not skippable, so a role check
// the app registered there is one an in-process caller cannot walk around by
// naming itself in Invocation.Auth.
func TestExecute_AuthStepAuthorizationBindsExecute(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(execSecret))
			s.Pipeline.Auth.Register(auth.RequireRole("admin"), maniflex.ForModel("Post"))
		},
	})

	// A principal without the role is refused, injected or not.
	_, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList,
		Auth: &maniflex.AuthInfo{UserID: "u1", Roles: []string{"viewer"}},
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 ExecuteError — an injected principal walked past "+
			"the Auth step's role check", err)
	}

	// ...and one with it is allowed.
	if _, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Post", Operation: maniflex.OpList,
		Auth: &maniflex.AuthInfo{UserID: "u2", Roles: []string{"admin"}},
	}); err != nil {
		t.Fatalf("Execute as admin: %v", err)
	}
}

// A forced filter constrains an Execute exactly as it constrains a request.
//
// Registered on the DB step, which is where db.ForceFilter/db.Tenancy go and the
// only place a query filter survives: the Deserialize step assigns ctx.Query
// wholesale, so a filter appended on Auth (before it) is discarded — over HTTP
// just as much as here. See P1-18 in the plan; it is a live bug in
// jobs/maniflex, not something Execute introduces.
func TestExecute_ForceFilterConstrainsExecute(t *testing.T) {
	scope := func(s *maniflex.Server) {
		s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			if ctx.Auth == nil {
				return next()
			}
			org, _ := ctx.Auth.Claims["org_id"].(string)
			ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
				Field: "org_id", Operator: maniflex.OpEq, Value: org, Forced: true,
			})
			return next()
		}, maniflex.ForModel("Article"))
	}
	srv := testutil.NewServer(t, testutil.Options{Models: []any{Article{}}, Middleware: scope})

	srv.MustID(srv.POST("/articles",
		map[string]any{"title": "a", "body": "B", "status": "draft", "org_id": "tenant-a"}))
	mine := srv.MustID(srv.POST("/articles",
		map[string]any{"title": "b", "body": "B", "status": "draft", "org_id": "tenant-b"}))

	asA := &maniflex.AuthInfo{UserID: "u1", Claims: map[string]any{"org_id": "tenant-a"}}

	res, err := srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Article", Operation: maniflex.OpList, Auth: asA,
	})
	if err != nil {
		t.Fatalf("Execute list: %v", err)
	}
	items := res.Data.([]any)
	if len(items) != 1 {
		t.Fatalf("list returned %d items, want 1 — the scope did not reach Execute", len(items))
	}
	if got := items[0].(map[string]any)["org_id"]; got != "tenant-a" {
		t.Errorf("visible row org_id = %v, want tenant-a", got)
	}

	// And the write half (P0-1) binds it too: an Execute must not be a way around
	// a scope that stops the same request over HTTP.
	_, err = srv.ManiflexServer().Execute(t.Context(), maniflex.Invocation{
		Model: "Article", Operation: maniflex.OpUpdate, ID: mine, Auth: asA,
		Body: map[string]any{"title": "PWNED"},
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-scope update through Execute: err = %v, want a 404 ExecuteError", err)
	}
}
