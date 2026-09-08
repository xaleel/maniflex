package openapi

// Audit OAS-2 — DocumentsResponse covers a status produced by a middleware the
// application registers, because the declaration can ride that middleware's own
// ForModel/ForOperation filters. AddResponse is the fallback for the rest: a
// handler that answers 402 itself, or a proxy in front of the server.

import (
	"testing"

	"github.com/xaleel/maniflex"
)

func specWithOrders() *maniflex.OpenAPISpec {
	return &maniflex.OpenAPISpec{
		Paths: map[string]maniflex.PathItem{
			"/orders": {
				Post: &maniflex.OASOperation{
					OperationID: "createOrder",
					Responses: map[string]maniflex.OASResponse{
						"201": {Description: "Order created"},
					},
				},
			},
		},
	}
}

// run drives the middleware the way the pipeline does: the spec exists by the
// time an After-registered middleware sees it.
func run(t *testing.T, mw maniflex.OpenAPIMiddlewareFunc, spec *maniflex.OpenAPISpec) {
	t.Helper()
	ctx := &maniflex.OpenAPIContext{Spec: spec}
	if err := mw(ctx, func() error { return nil }); err != nil {
		t.Fatalf("middleware returned %v", err)
	}
}

func TestAddResponse_DocumentsAStatusOnOneOperation(t *testing.T) {
	t.Parallel()

	spec := specWithOrders()
	run(t, AddResponse(OperationTarget{Path: "/orders", Method: "post"},
		402, "Payment required", &maniflex.OASSchema{
			Type:       "object",
			Properties: map[string]*maniflex.OASSchema{"invoice": {Type: "string"}},
		}), spec)

	resp, ok := spec.Paths["/orders"].Post.Responses["402"]
	if !ok {
		t.Fatal("402 was not added")
	}
	if resp.Description != "Payment required" {
		t.Errorf("description = %q", resp.Description)
	}
	if resp.Content["application/json"].Schema.Properties["invoice"] == nil {
		t.Error("the schema did not reach the response")
	}
	// The operation's own responses must survive.
	if _, ok := spec.Paths["/orders"].Post.Responses["201"]; !ok {
		t.Error("the generated 201 was lost")
	}
}

func TestAddResponse_NilSchemaMeansNoBody(t *testing.T) {
	t.Parallel()

	spec := specWithOrders()
	run(t, AddResponse(OperationTarget{Path: "/orders", Method: "post"},
		402, "Payment required", nil), spec)

	if got := spec.Paths["/orders"].Post.Responses["402"].Content; got != nil {
		t.Errorf("content = %v, want nil for a bodyless response", got)
	}
}

// The cost of addressing by path string, made explicit: a target that no longer
// resolves is silently ignored rather than failing the request that generates
// the spec. This is why DocumentsResponse is the better seam when it applies.
func TestAddResponse_UnknownTargetIsIgnored(t *testing.T) {
	t.Parallel()

	spec := specWithOrders()
	run(t, AddResponse(OperationTarget{Path: "/renamed", Method: "post"},
		402, "Payment required", nil), spec)
	run(t, AddResponse(OperationTarget{Path: "/orders", Method: "delete"},
		402, "Payment required", nil), spec)

	if len(spec.Paths["/orders"].Post.Responses) != 1 {
		t.Errorf("responses = %v, want the original 201 alone",
			spec.Paths["/orders"].Post.Responses)
	}
}

func TestAddResponse_ReplacesAnExistingStatus(t *testing.T) {
	t.Parallel()

	spec := specWithOrders()
	run(t, AddResponse(OperationTarget{Path: "/orders", Method: "post"},
		201, "Order accepted for fulfilment", nil), spec)

	if got := spec.Paths["/orders"].Post.Responses["201"].Description; got != "Order accepted for fulfilment" {
		t.Errorf("description = %q; an explicit override should win", got)
	}
}
