package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type HeadlessThread struct {
	maniflex.BaseModel
	Title string `json:"title" mfx:"filterable"`
}

// A Headless model registers fully but mounts no REST routes, so a custom action
// can own the model's table path without the chi route collision that would
// otherwise panic at boot. Regression / feature test for ModelConfig.Headless.
func TestHeadlessModel_FreesPathForAction(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{HeadlessThread{}, maniflex.ModelConfig{Headless: true}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: http.MethodGet,
				Path:   "/headless_threads",
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{
						StatusCode: http.StatusOK,
						Data:       map[string]any{"from_action": true},
					}
					return nil
				},
			})
		},
	})

	// The server booted (no model-vs-action route collision) and the action owns
	// GET /headless_threads.
	data := srv.GET("/headless_threads").AssertStatus(http.StatusOK).Data()
	if data["from_action"] != true {
		t.Fatalf("expected the custom action to serve the path, got %v", data)
	}

	// The model's auto CRUD was not mounted: POST is not a create route.
	resp := srv.POST("/headless_threads", map[string]any{"title": "x"})
	if resp.Status != http.StatusMethodNotAllowed && resp.Status != http.StatusNotFound {
		t.Fatalf("headless model must not mount a create route; got status %d\nbody: %s", resp.Status, resp.Body)
	}
}
