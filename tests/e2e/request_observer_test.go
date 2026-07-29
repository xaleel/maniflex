package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestRequestObserverCapturesAuthRejectionAndFullDuration(t *testing.T) {
	const authDelay = 40 * time.Millisecond
	observed := make(chan maniflex.RequestObservation, 1)

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(server *maniflex.Server) {
			server.ObserveRequests(func(observation maniflex.RequestObservation) {
				observed <- observation
			})
			server.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, _ func() error) error {
				time.Sleep(authDelay)
				ctx.Auth = &maniflex.AuthInfo{
					UserID: "blocked-user",
					Claims: map[string]any{},
				}
				ctx.Abort(http.StatusForbidden, "FORBIDDEN", "blocked in auth")
				return nil
			})
		},
	})

	const id = "00000000-0000-0000-0000-000000000042"
	srv.GET("/users/" + id).AssertStatus(http.StatusForbidden)

	select {
	case observation := <-observed:
		if observation.Status != http.StatusForbidden {
			t.Errorf("status = %d, want 403", observation.Status)
		}
		if observation.Model != "User" {
			t.Errorf("model = %q, want User", observation.Model)
		}
		if observation.Operation != maniflex.OpRead {
			t.Errorf("operation = %q, want read", observation.Operation)
		}
		if observation.ResourceID != id {
			t.Errorf("resource ID = %q, want %q", observation.ResourceID, id)
		}
		if observation.Actor != "blocked-user" {
			t.Errorf("actor = %q, want blocked-user", observation.Actor)
		}
		if observation.RequestID == "" {
			t.Error("request ID is empty")
		}
		if observation.Path != "/api/users/"+id {
			t.Errorf("path = %q", observation.Path)
		}
		if observation.Duration < authDelay {
			t.Errorf("duration = %s, want at least auth delay %s", observation.Duration, authDelay)
		}
	case <-time.After(time.Second):
		t.Fatal("request observer was not called")
	}
}
