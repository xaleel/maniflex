package maniflex

// Audit OAS-2 — model operations documented only the statuses their own shape
// implied, so a server with auth on Pipeline.Auth documented 401 on a custom
// action (buildActionResponses hands one out by default) and denied it on the
// model route behind the same guard. 409, 412, 503 and 504 were absent
// everywhere, and OASResponse had no Headers field at all, which left the ETag /
// If-Match handshake unexpressible even by hand.

import (
	"testing"
	"time"
)

type oasRespModel struct {
	BaseModel
	Title string `json:"title" mfx:"unique"`
}

type oasPlainModel struct {
	BaseModel
	Note string `json:"note"`
}

func passthrough(ctx *ServerContext, next func() error) error { return next() }

// specFor builds a spec the way the server does — through the live pipeline, so
// a test cannot accidentally assert on a document the server would never serve.
func specFor(t *testing.T, cfg Config, build func(*Server)) *OpenAPISpec {
	t.Helper()
	cfg.PathPrefix = "/api"
	srv := New(cfg)
	build(srv)
	return GenerateSpec(srv.Registry(), &srv.cfg, srv.actions, srv.Pipeline)
}

func opFor(t *testing.T, spec *OpenAPISpec, path, method string) *OASOperation {
	t.Helper()
	item, ok := spec.Paths[path]
	if !ok {
		t.Fatalf("path %q absent from the spec", path)
	}
	var op *OASOperation
	switch method {
	case "get":
		op = item.Get
	case "post":
		op = item.Post
	case "patch":
		op = item.Patch
	case "delete":
		op = item.Delete
	}
	if op == nil {
		t.Fatalf("%s %s absent from the spec", method, path)
	}
	return op
}

func hasStatus(op *OASOperation, status string) bool {
	_, ok := op.Responses[status]
	return ok
}

// The inconsistency the finding is about: one guard, one server, and the spec
// used to admit it on the action alone.
func TestOpenAPIResponses_AuthStepImpliesUnauthorized(t *testing.T) {
	t.Parallel()

	guarded := specFor(t, Config{}, func(s *Server) {
		s.MustRegister(oasRespModel{})
		s.Pipeline.Auth.Register(passthrough)
		s.Action(ActionConfig{Method: "POST", Path: "/publish",
			Handler: func(*ServerContext) error { return nil }})
	})

	for _, tc := range []struct{ path, method string }{
		{"/oas_resp_models", "get"}, {"/oas_resp_models", "post"},
		{"/oas_resp_models/{id}", "get"}, {"/oas_resp_models/{id}", "patch"},
		{"/oas_resp_models/{id}", "delete"},
		{"/publish", "post"},
	} {
		op := opFor(t, guarded, tc.path, tc.method)
		for _, status := range []string{"401", "403"} {
			if !hasStatus(op, status) {
				t.Errorf("%s %s does not document %s, but the Auth step guards it",
					tc.method, tc.path, status)
			}
		}
	}
}

// The other direction, which a fixed common set would have got wrong: a server
// with no auth must not tell clients to handle 401.
func TestOpenAPIResponses_NoAuthMeansNoUnauthorized(t *testing.T) {
	t.Parallel()

	open := specFor(t, Config{}, func(s *Server) { s.MustRegister(oasRespModel{}) })

	op := opFor(t, open, "/oas_resp_models", "get")
	for _, status := range []string{"401", "403"} {
		if hasStatus(op, status) {
			t.Errorf("%s documented on a server with no Auth middleware", status)
		}
	}
	// 500 is the one that is always reachable.
	if !hasStatus(op, "500") {
		t.Error("500 is not documented; any operation can fail")
	}
}

// The point of declaring on the registration rather than on the model: the
// status lands on exactly the operations the middleware runs on.
func TestOpenAPIResponses_DocumentsResponseInheritsMiddlewareScoping(t *testing.T) {
	t.Parallel()

	spec := specFor(t, Config{}, func(s *Server) {
		s.MustRegister(oasRespModel{}, oasPlainModel{})
		s.Pipeline.Validate.Register(passthrough,
			ForModel("oasRespModel"), ForOperation(OpCreate),
			DocumentsResponse(402, "Payment required", nil),
		)
	})

	if op := opFor(t, spec, "/oas_resp_models", "post"); !hasStatus(op, "402") {
		t.Error("the declared 402 is missing from the operation it was scoped to")
	}
	for _, tc := range []struct{ path, method string }{
		{"/oas_resp_models", "get"},        // wrong operation
		{"/oas_resp_models/{id}", "patch"}, // wrong operation
		{"/oas_plain_models", "post"},      // wrong model
	} {
		if op := opFor(t, spec, tc.path, tc.method); hasStatus(op, "402") {
			t.Errorf("%s %s documents 402, but the middleware does not run there",
				tc.method, tc.path)
		}
	}
}

func TestOpenAPIResponses_DeclaredSchemaReachesTheDocument(t *testing.T) {
	t.Parallel()

	spec := specFor(t, Config{}, func(s *Server) {
		s.MustRegister(oasRespModel{})
		s.Pipeline.Validate.Register(passthrough,
			DocumentsResponse(409, "Already taken", &OASSchema{
				Type:       "object",
				Properties: map[string]*OASSchema{"field": {Type: "string"}},
			}),
		)
	})

	resp := opFor(t, spec, "/oas_resp_models", "post").Responses["409"]
	if resp.Description != "Already taken" {
		t.Errorf("description = %q, want the declared one", resp.Description)
	}
	media, ok := resp.Content["application/json"]
	if !ok || media.Schema == nil || media.Schema.Properties["field"] == nil {
		t.Errorf("the declared schema did not reach the document: %+v", resp.Content)
	}
}

// Both halves of the conditional-write handshake, and only where the DB step
// actually performs one.
func TestOpenAPIResponses_OptimisticLockDocumentsBothHalves(t *testing.T) {
	t.Parallel()

	locked := specFor(t, Config{}, func(s *Server) {
		s.MustRegister(oasRespModel{}, ModelConfig{OptimisticLock: true})
	})

	read := opFor(t, locked, "/oas_resp_models/{id}", "get")
	if _, ok := read.Responses["200"].Headers["ETag"]; !ok {
		t.Error("the read does not document the ETag a caller needs for If-Match")
	}
	for _, method := range []string{"patch", "delete"} {
		op := opFor(t, locked, "/oas_resp_models/{id}", method)
		if !hasStatus(op, "412") {
			t.Errorf("%s does not document 412", method)
		}
		var found bool
		for _, p := range op.Parameters {
			if p.In == "header" && p.Name == "If-Match" {
				found = true
				// Absent If-Match bypasses the check rather than failing, so
				// declaring it required would misstate the contract.
				if p.Required {
					t.Errorf("%s declares If-Match required; omitting it is legal", method)
				}
			}
		}
		if !found {
			t.Errorf("%s does not document the If-Match request header", method)
		}
	}

	unlocked := specFor(t, Config{}, func(s *Server) { s.MustRegister(oasRespModel{}) })
	if op := opFor(t, unlocked, "/oas_resp_models/{id}", "patch"); hasStatus(op, "412") {
		t.Error("412 documented on a model without OptimisticLock")
	}
	if _, ok := opFor(t, unlocked, "/oas_resp_models/{id}", "get").
		Responses["200"].Headers["ETag"]; ok {
		t.Error("ETag documented on a model without OptimisticLock")
	}
}

func TestOpenAPIResponses_LimitsDocumentedOnlyWhenConfigured(t *testing.T) {
	t.Parallel()

	reg := func(s *Server) { s.MustRegister(oasRespModel{}) }

	off := specFor(t, Config{}, reg)
	offOp := opFor(t, off, "/oas_resp_models", "get")
	if hasStatus(offOp, "503") {
		t.Error("503 documented with MaxConcurrentRequests unset — the limiter is off")
	}
	if hasStatus(offOp, "504") {
		t.Error("504 documented with QueryTimeout unset — nothing bounds the query")
	}

	on := specFor(t, Config{MaxConcurrentRequests: 8, QueryTimeout: 5 * time.Second}, reg)
	onOp := opFor(t, on, "/oas_resp_models", "get")
	if !hasStatus(onOp, "503") {
		t.Error("503 not documented with MaxConcurrentRequests set")
	}
	if !hasStatus(onOp, "504") {
		t.Error("504 not documented with QueryTimeout set")
	}
	// Retry-After is the only part of a 503 a client can act on.
	if _, ok := onOp.Responses["503"].Headers["Retry-After"]; !ok {
		t.Error("the 503 does not document Retry-After")
	}
}

func TestOpenAPIResponses_ConflictFollowsTheConstraints(t *testing.T) {
	t.Parallel()

	unique := specFor(t, Config{}, func(s *Server) { s.MustRegister(oasRespModel{}) })
	for _, method := range []string{"post"} {
		if !hasStatus(opFor(t, unique, "/oas_resp_models", method), "409") {
			t.Errorf("%s does not document 409 on a model with a unique field", method)
		}
	}

	plain := specFor(t, Config{}, func(s *Server) { s.MustRegister(oasPlainModel{}) })
	if hasStatus(opFor(t, plain, "/oas_plain_models", "post"), "409") {
		t.Error("409 documented on a model with no unique field and no relations")
	}
	if hasStatus(opFor(t, plain, "/oas_plain_models/{id}", "delete"), "409") {
		t.Error("409 documented on a delete with no restrict relation to refuse it")
	}
}

// The generated 404 says which record was not found; a derived one would be a
// downgrade, so shape-declared responses must win.
func TestOpenAPIResponses_DerivedDoNotDisplaceDeclared(t *testing.T) {
	t.Parallel()

	spec := specFor(t, Config{}, func(s *Server) {
		s.MustRegister(oasRespModel{})
		s.Pipeline.Auth.Register(passthrough)
	})

	got := opFor(t, spec, "/oas_resp_models/{id}", "get").Responses["404"].Description
	if got != "oasRespModel not found" {
		t.Errorf("404 description = %q; the model-specific one was overwritten", got)
	}
}

// GenerateSpec is exported and a caller assembling a document by hand has no
// pipeline to offer. That must describe the models rather than panic.
func TestOpenAPIResponses_NilPipelineIsUsable(t *testing.T) {
	t.Parallel()

	srv := New(Config{PathPrefix: "/api"})
	srv.MustRegister(oasRespModel{})
	srv.Pipeline.Auth.Register(passthrough)

	spec := GenerateSpec(srv.Registry(), &srv.cfg, nil, nil)
	op := opFor(t, spec, "/oas_resp_models", "get")
	if !hasStatus(op, "200") || !hasStatus(op, "500") {
		t.Error("a nil pipeline should still produce the model's own responses")
	}
	if hasStatus(op, "401") {
		t.Error("401 cannot be known without the pipeline")
	}
}
