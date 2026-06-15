package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Ledger is a registered model; AutoModelEvents should derive
// ledger.created|updated|deleted channels from it (10.9 / workstream C).
type Ledger struct {
	maniflex.BaseModel
	Number string  `json:"number"`
	Amount float64 `json:"amount"`
}

// aaPaymentReceived is a custom (non-model) event payload declared via EventDoc.
type aaPaymentReceived struct {
	InvoiceID string  `json:"invoice_id" mfx:"required"`
	Amount    float64 `json:"amount"`
	Method    string  `json:"method" mfx:"enum=card|cash"`
}

func TestAsyncAPI_ModelAndCustomEvents(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Ledger{}},
		Middleware: func(s *maniflex.Server) {
			s.RealtimeDoc(maniflex.AsyncAPIConfig{
				Title:           "Billing Events",
				Version:         "2.0.0",
				AutoModelEvents: true,
				Servers: []maniflex.AsyncAPIServerConfig{
					{Name: "ws", URL: "ws://localhost:8080/ws", Protocol: "ws"},
				},
				Events: []maniflex.EventDoc{
					{Type: "payment.received", Title: "Payment received", Payload: aaPaymentReceived{}},
				},
			})
		},
	})

	resp := srv.Do(http.MethodGet, srv.APIPath("/asyncapi.json"), nil)
	resp.AssertStatus(http.StatusOK)

	var doc map[string]any
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		t.Fatalf("parse asyncapi doc: %v", err)
	}

	if doc["asyncapi"] != "2.6.0" {
		t.Errorf("asyncapi = %v, want 2.6.0", doc["asyncapi"])
	}
	info := doc["info"].(map[string]any)
	if info["title"] != "Billing Events" || info["version"] != "2.0.0" {
		t.Errorf("info = %v", info)
	}

	// Server block.
	servers, _ := doc["servers"].(map[string]any)
	ws, ok := servers["ws"].(map[string]any)
	if !ok {
		t.Fatalf("ws server missing; servers=%v", servers)
	}
	if ws["protocol"] != "ws" || ws["url"] == "" {
		t.Errorf("ws server = %v", ws)
	}

	channels := doc["channels"].(map[string]any)
	for _, typ := range []string{"ledger.created", "ledger.updated", "ledger.deleted", "payment.received"} {
		if _, ok := channels[typ]; !ok {
			t.Errorf("channel %q missing; have %v", typ, keysOf(channels))
		}
	}

	// ledger.created → subscribe → message $ref → Ledger schema.
	created := channels["ledger.created"].(map[string]any)
	sub, ok := created["subscribe"].(map[string]any)
	if !ok {
		t.Fatalf("ledger.created has no subscribe op: %v", created)
	}
	msgRef := sub["message"].(map[string]any)["$ref"].(string)
	if msgRef != "#/components/messages/ledger.created" {
		t.Errorf("message $ref = %q", msgRef)
	}

	components := doc["components"].(map[string]any)
	messages := components["messages"].(map[string]any)
	schemas := components["schemas"].(map[string]any)

	// The model event payload references the reflected Ledger schema.
	ledgerMsg := messages["ledger.created"].(map[string]any)
	payloadRef := ledgerMsg["payload"].(map[string]any)["$ref"].(string)
	if payloadRef != "#/components/schemas/Ledger" {
		t.Errorf("ledger.created payload $ref = %q", payloadRef)
	}
	if _, ok := schemas["Ledger"]; !ok {
		t.Errorf("Ledger schema missing; have %v", keysOf(schemas))
	}

	// The custom event reflected its Go struct, honouring mfx tags.
	paySchema, ok := schemas["aaPaymentReceived"].(map[string]any)
	if !ok {
		t.Fatalf("aaPaymentReceived schema missing; have %v", keysOf(schemas))
	}
	props := paySchema["properties"].(map[string]any)
	if _, ok := props["invoice_id"]; !ok {
		t.Error("aaPaymentReceived.invoice_id property missing")
	}
	if req, _ := paySchema["required"].([]any); len(req) == 0 || req[0] != "invoice_id" {
		t.Errorf("aaPaymentReceived.required = %v, want [invoice_id]", paySchema["required"])
	}

	assertNoDanglingAsyncRefs(t, doc)
}

// TestAsyncAPI_EndpointAbsentWithoutRealtimeDoc verifies the endpoint is opt-in:
// apps that never call RealtimeDoc gain no /asyncapi.json route.
func TestAsyncAPI_EndpointAbsentWithoutRealtimeDoc(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{Ledger{}}})
	resp := srv.Do(http.MethodGet, srv.APIPath("/asyncapi.json"), nil)
	resp.AssertStatus(http.StatusNotFound)
}

// assertNoDanglingAsyncRefs walks every "$ref" in the document and asserts the
// referenced component (message or schema) exists.
func assertNoDanglingAsyncRefs(t *testing.T, doc map[string]any) {
	t.Helper()
	components, _ := doc["components"].(map[string]any)
	messages, _ := components["messages"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)

	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, val := range n {
				if k == "$ref" {
					ref, _ := val.(string)
					switch {
					case strings.HasPrefix(ref, "#/components/messages/"):
						name := strings.TrimPrefix(ref, "#/components/messages/")
						if _, ok := messages[name]; !ok {
							t.Errorf("dangling message $ref: %s", ref)
						}
					case strings.HasPrefix(ref, "#/components/schemas/"):
						name := strings.TrimPrefix(ref, "#/components/schemas/")
						if _, ok := schemas[name]; !ok {
							t.Errorf("dangling schema $ref: %s", ref)
						}
					default:
						t.Errorf("unexpected $ref form: %s", ref)
					}
				}
				walk(val)
			}
		case []any:
			for _, e := range n {
				walk(e)
			}
		}
	}
	walk(doc)
}
