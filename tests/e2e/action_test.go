package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestActionBasic verifies that a POST action returns custom JSON.
func TestActionBasic(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/ping",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"pong": true},
					}
					return nil
				},
			})
		},
	})

	data := srv.POST("/ping", nil).AssertStatus(http.StatusOK).Data()
	if data["pong"] != true {
		t.Errorf("expected pong:true, got %v", data)
	}
}

// TestActionGetWithQueryParam verifies GET action reads ctx.QueryParam.
func TestActionGetWithQueryParam(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/greet",
				Handler: func(ctx *maniflex.ServerContext) error {
					name := ctx.QueryParam("name")
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"hello": name},
					}
					return nil
				},
			})
		},
	})

	data := srv.Do(http.MethodGet, srv.APIPath("/greet")+"?name=Alice", nil).
		AssertStatus(http.StatusOK).Data()
	if data["hello"] != "Alice" {
		t.Errorf("expected hello:Alice, got %v", data)
	}
}

// TestActionBindJSON verifies ctx.BindJSON parses the body, and that a missing
// body returns 400 EMPTY_BODY.
func TestActionBindJSON(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/echo",
				Handler: func(ctx *maniflex.ServerContext) error {
					var req struct {
						Msg string `json:"msg"`
					}
					if err := ctx.BindJSON(&req); err != nil {
						return nil
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"echoed": req.Msg},
					}
					return nil
				},
			})
		},
	})

	t.Run("valid body", func(t *testing.T) {
		data := srv.POST("/echo", map[string]any{"msg": "hello"}).
			AssertStatus(http.StatusOK).Data()
		if data["echoed"] != "hello" {
			t.Errorf("expected echoed:hello, got %v", data)
		}
	})

	t.Run("missing body returns 400", func(t *testing.T) {
		resp := srv.Do(http.MethodPost, srv.APIPath("/echo"), nil)
		resp.AssertStatus(http.StatusBadRequest)
		if code := resp.ErrorCode(); code != "EMPTY_BODY" {
			t.Errorf("expected EMPTY_BODY, got %q", code)
		}
	})
}

// TestActionBindJSONSizeLimit verifies that a body larger than 4 MB is rejected.
func TestActionBindJSONSizeLimit(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/big",
				Handler: func(ctx *maniflex.ServerContext) error {
					var v map[string]any
					if err := ctx.BindJSON(&v); err != nil {
						return nil
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	// Build a JSON string with a value > 4 MB
	bigVal := strings.Repeat("x", 4<<20+1)
	body, _ := json.Marshal(map[string]any{"data": bigVal})

	req, _ := http.NewRequest(http.MethodPost, srv.APIPath("/big"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized body, got %d", httpResp.StatusCode)
	}

	// An oversized body gets a dedicated signal rather than being silently
	// truncated into a confusing INVALID_JSON parse failure.
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&env); err != nil {
		t.Fatalf("parse response body: %v", err)
	}
	if env.Error.Code != "BODY_TOO_LARGE" {
		t.Errorf("expected BODY_TOO_LARGE, got %q", env.Error.Code)
	}
}

// TestActionResourceID verifies ctx.ResourceID is populated from {id}.
func TestActionResourceID(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/items/{id}/activate",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"activated": ctx.ResourceID},
					}
					return nil
				},
			})
		},
	})

	data := srv.POST("/items/abc-123/activate", nil).AssertStatus(http.StatusOK).Data()
	if data["activated"] != "abc-123" {
		t.Errorf("expected activated:abc-123, got %v", data)
	}
}

// TestActionURLParam verifies ctx.URLParam reads named path params beyond {id}.
func TestActionURLParam(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET",
				Path:   "/wards/{wardId}/beds/{bedId}",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data: map[string]any{
							"ward": ctx.URLParam("wardId"),
							"bed":  ctx.URLParam("bedId"),
						},
					}
					return nil
				},
			})
		},
	})

	data := srv.Do(http.MethodGet, srv.APIPath("/wards/W1/beds/B2"), nil).
		AssertStatus(http.StatusOK).Data()
	if data["ward"] != "W1" || data["bed"] != "B2" {
		t.Errorf("unexpected params: %v", data)
	}
}

// TestActionGlobalAuth verifies that global Auth middleware applies to actions.
func TestActionGlobalAuth(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			// Global auth middleware: require X-Token header
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Request.Header.Get("X-Token") == "" {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "missing token")
					return nil
				}
				return next()
			})
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/secure",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	t.Run("no token → 401", func(t *testing.T) {
		srv.POST("/secure", nil).AssertStatus(http.StatusUnauthorized)
	})

	t.Run("with token → 200", func(t *testing.T) {
		srv.Do(http.MethodPost, srv.APIPath("/secure"), nil, map[string]string{"X-Token": "secret"}).
			AssertStatus(http.StatusOK)
	})
}

// TestActionPerActionMiddleware verifies that per-action middleware runs and can abort.
func TestActionPerActionMiddleware(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			requireAdmin := func(ctx *maniflex.ServerContext, next func() error) error {
				if ctx.Request.Header.Get("X-Role") != "admin" {
					ctx.Abort(http.StatusForbidden, "FORBIDDEN", "admin only")
					return nil
				}
				return next()
			}
			s.Action(maniflex.ActionConfig{
				Method:     "DELETE",
				Path:       "/nuke",
				Middleware: []maniflex.MiddlewareFunc{requireAdmin},
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	t.Run("non-admin → 403", func(t *testing.T) {
		srv.Do(http.MethodDelete, srv.APIPath("/nuke"), nil).AssertStatus(http.StatusForbidden)
	})

	t.Run("admin → 200", func(t *testing.T) {
		srv.Do(http.MethodDelete, srv.APIPath("/nuke"), nil, map[string]string{"X-Role": "admin"}).
			AssertStatus(http.StatusOK)
	})
}

// TestActionMissingResponse verifies that if the handler sets no ctx.Response,
// the Response step defaults to 200 OK with no body (not a panic).
func TestActionMissingResponse(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/noop",
				Handler: func(ctx *maniflex.ServerContext) error {
					// intentionally sets nothing
					return nil
				},
			})
		},
	})

	srv.POST("/noop", nil).AssertStatus(http.StatusOK)
}

// TestActionNotInCRUD verifies that an action on a distinct path does not
// shadow or break the CRUD routes for the same model.
func TestActionNotInCRUD(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/users/summary",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"action": true},
					}
					return nil
				},
			})
		},
	})

	// Action path works
	data := srv.POST("/users/summary", nil).AssertStatus(http.StatusOK).Data()
	if data["action"] != true {
		t.Errorf("expected action:true, got %v", data)
	}

	// CRUD POST /users still works independently
	srv.POST("/users", map[string]any{
		"name": "Alice", "email": "alice@x.com", "password": "s",
	}).AssertStatus(http.StatusCreated)
}

// TestActionConflictPanics verifies that registering an action that conflicts
// with an auto-generated model route causes a panic.
func TestActionConflictPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for conflicting action, got none")
		}
	}()

	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(testutil.User{})
	// POST /users conflicts with the auto-generated create route
	server.Action(maniflex.ActionConfig{
		Method:  "POST",
		Path:    "/users",
		Handler: func(ctx *maniflex.ServerContext) error { return nil },
	})
}

// TestActionAfterStartPanics verifies that calling Action() after Handler()
// has been called causes a panic.
func TestActionAfterStartPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for late Action() call, got none")
		}
	}()

	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(testutil.User{})
	_ = server.Handler() // triggers router build
	server.Action(maniflex.ActionConfig{
		Method:  "POST",
		Path:    "/late",
		Handler: func(ctx *maniflex.ServerContext) error { return nil },
	})
}

// TestActionOpenAPI verifies that a registered action appears in the OpenAPI
// spec under the correct path, method, tag, and with path parameters extracted.
func TestActionOpenAPI(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:  "POST",
				Path:    "/appointments/{id}/cancel",
				Tags:    []string{"Appointments"},
				Summary: "Cancel an appointment",
				Handler: func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/openapi.json"), nil)
	resp.AssertStatus(http.StatusOK)

	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)
	// Action paths are stored relative to the server URL (which carries the
	// PathPrefix), exactly like model paths — not prefixed a second time.
	specPath := "/appointments/{id}/cancel"
	pathItem, ok := paths[specPath].(map[string]any)
	if !ok {
		t.Fatalf("expected path %q in spec, got paths: %v", specPath, keys(paths))
	}

	post, ok := pathItem["post"].(map[string]any)
	if !ok {
		t.Fatalf("expected post operation at %q", specPath)
	}

	if post["summary"] != "Cancel an appointment" {
		t.Errorf("summary: got %v", post["summary"])
	}

	tags, _ := post["tags"].([]any)
	if len(tags) == 0 || tags[0] != "Appointments" {
		t.Errorf("tags: got %v", tags)
	}

	params, _ := post["parameters"].([]any)
	if len(params) == 0 {
		t.Errorf("expected path param {id} in parameters")
	} else {
		p0, _ := params[0].(map[string]any)
		if p0["name"] != "id" || p0["in"] != "path" {
			t.Errorf("unexpected param: %v", p0)
		}
	}
}

// TestActionOpenAPIDeprecated verifies that Deprecated:true appears in the spec.
func TestActionOpenAPIDeprecated(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:     "POST",
				Path:       "/legacy",
				Deprecated: true,
				Handler:    func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/openapi.json"), nil)
	resp.AssertStatus(http.StatusOK)

	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	paths, _ := spec["paths"].(map[string]any)
	pathItem, _ := paths["/legacy"].(map[string]any)
	post, _ := pathItem["post"].(map[string]any)
	if post["deprecated"] != true {
		t.Errorf("expected deprecated:true, got %v", post["deprecated"])
	}
}

// TestActionTransaction verifies that ctx.BeginTx wires up correctly: a
// committed transaction persists a record, a rolled-back one does not.
func TestActionTransaction(t *testing.T) {
	t.Parallel()

	// Capture the User model meta via the registry so we can use it inside
	// the action handler (which doesn't have direct access to the registry).
	var userMeta *maniflex.ModelMeta

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			m, ok := s.Registry().Get("User")
			if !ok {
				t.Fatal("User model not found in registry")
			}
			userMeta = m

			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/tx-commit",
				Handler: func(ctx *maniflex.ServerContext) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()

					_, err = tx.Create(ctx.Ctx, userMeta, map[string]any{
						"name":     "TxUser",
						"email":    "txuser@x.com",
						"password": "s",
						"role":     "viewer",
					})
					if err != nil {
						return err
					}
					if err := tx.Commit(); err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})

			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/tx-rollback",
				Handler: func(ctx *maniflex.ServerContext) error {
					tx, err := ctx.BeginTx(ctx.Ctx, nil)
					if err != nil {
						return err
					}
					defer tx.Rollback() // always rolls back — no Commit

					_, err = tx.Create(ctx.Ctx, userMeta, map[string]any{
						"name":     "RollbackUser",
						"email":    "rollback@x.com",
						"password": "s",
						"role":     "viewer",
					})
					if err != nil {
						return err
					}
					// deliberately no Commit — Rollback fires in defer
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	// Commit path: user must appear in list
	srv.POST("/tx-commit", nil).AssertStatus(http.StatusOK)
	listResp := srv.GET("/users")
	listResp.AssertStatus(http.StatusOK)
	items := listResp.DataList()
	found := false
	for _, item := range items {
		u, _ := item.(map[string]any)
		if u["email"] == "txuser@x.com" {
			found = true
		}
	}
	if !found {
		t.Error("committed user not found in list")
	}

	// Rollback path: user must NOT appear in list
	srv.POST("/tx-rollback", nil).AssertStatus(http.StatusOK)
	items = srv.GET("/users").DataList()
	for _, item := range items {
		u, _ := item.(map[string]any)
		if u["email"] == "rollback@x.com" {
			t.Error("rolled-back user should not appear in list")
		}
	}
}

// keys is a test helper to extract map keys for error messages.
func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestActionNilHandlerPanics verifies that registering an action with a nil
// Handler panics at registration time rather than on the first request (A2).
func TestActionNilHandlerPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil Handler, got none")
		}
	}()

	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(testutil.User{})
	server.Action(maniflex.ActionConfig{Method: "POST", Path: "/nohandler"})
}

// TestActionMissingMethodOrPathPanics verifies that an empty Method or Path is
// rejected at registration time (A2).
func TestActionMissingMethodOrPathPanics(t *testing.T) {
	t.Parallel()

	cases := map[string]maniflex.ActionConfig{
		"empty method": {Path: "/x", Handler: func(*maniflex.ServerContext) error { return nil }},
		"empty path":   {Method: "POST", Handler: func(*maniflex.ServerContext) error { return nil }},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s, got none", name)
				}
			}()
			server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
			server.MustRegister(testutil.User{})
			server.Action(cfg)
		})
	}
}

// TestActionDuplicatePanics verifies that two actions with the same method+path
// are rejected at registration rather than silently shadowed by chi (A3).
func TestActionDuplicatePanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for duplicate action, got none")
		}
	}()

	h := func(*maniflex.ServerContext) error { return nil }
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(testutil.User{})
	server.Action(maniflex.ActionConfig{Method: "POST", Path: "/dup", Handler: h})
	// Trailing slash must normalise to the same path → still a duplicate.
	server.Action(maniflex.ActionConfig{Method: "post", Path: "/dup/", Handler: h})
}

// TestActionResponseMiddleware verifies that ResponseMiddleware runs after the
// handler and can post-process ctx.Response before it is written (A4).
func TestActionResponseMiddleware(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/rmw",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"stage": "handler"},
					}
					return nil
				},
				ResponseMiddleware: []maniflex.MiddlewareFunc{
					func(ctx *maniflex.ServerContext, next func() error) error {
						// Runs after the handler set ctx.Response.
						if ctx.Response != nil {
							ctx.Response.Data = map[string]any{"stage": "response_mw"}
						}
						return next()
					},
				},
			})
		},
	})

	data := srv.POST("/rmw", nil).AssertStatus(http.StatusOK).Data()
	if data["stage"] != "response_mw" {
		t.Errorf("expected ResponseMiddleware to overwrite data, got %v", data)
	}
}

// TestActionTryBindJSON verifies the optional-body contract of ctx.TryBindJSON:
// an absent body is not an error, a present body parses, and malformed JSON is
// rejected with 400 (A5).
func TestActionTryBindJSON(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/optional",
				Handler: func(ctx *maniflex.ServerContext) error {
					var req struct {
						Msg string `json:"msg"`
					}
					ok, err := ctx.TryBindJSON(&req)
					if err != nil {
						return nil // ctx.Abort already called
					}
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"present": ok, "msg": req.Msg},
					}
					return nil
				},
			})
		},
	})

	t.Run("absent body is not an error", func(t *testing.T) {
		data := srv.Do(http.MethodPost, srv.APIPath("/optional"), nil).
			AssertStatus(http.StatusOK).Data()
		if data["present"] != false || data["msg"] != "" {
			t.Errorf("expected present:false msg:\"\", got %v", data)
		}
	})

	t.Run("present body parses", func(t *testing.T) {
		data := srv.POST("/optional", map[string]any{"msg": "hi"}).
			AssertStatus(http.StatusOK).Data()
		if data["present"] != true || data["msg"] != "hi" {
			t.Errorf("expected present:true msg:hi, got %v", data)
		}
	})

	t.Run("malformed body returns 400 INVALID_JSON", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.APIPath("/optional"),
			bytes.NewReader([]byte("{not json")))
		req.Header.Set("Content-Type", "application/json")
		httpResp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer httpResp.Body.Close()
		if httpResp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", httpResp.StatusCode)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.NewDecoder(httpResp.Body).Decode(&env); err != nil {
			t.Fatalf("parse response body: %v", err)
		}
		if env.Error.Code != "INVALID_JSON" {
			t.Errorf("expected INVALID_JSON, got %q", env.Error.Code)
		}
	})
}

// TestActionOpenAPIDefaultTag verifies that when an action uses the default
// "Actions" tag, a matching entry is declared in the document's tag list (A6).
func TestActionOpenAPIDefaultTag(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.User{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method:  "POST",
				Path:    "/untagged",
				Handler: func(ctx *maniflex.ServerContext) error { return nil },
			})
		},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/openapi.json"), nil)
	resp.AssertStatus(http.StatusOK)

	var spec map[string]any
	if err := json.Unmarshal(resp.Body, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	specTags, _ := spec["tags"].([]any)
	found := false
	for _, raw := range specTags {
		tag, _ := raw.(map[string]any)
		if tag["name"] == "Actions" {
			found = true
			if tag["description"] == "" || tag["description"] == nil {
				t.Errorf("Actions tag should carry a description, got %v", tag["description"])
			}
		}
	}
	if !found {
		t.Errorf("expected an \"Actions\" entry in spec.tags, got %v", specTags)
	}
}
