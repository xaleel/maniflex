package e2e_test

// Regression coverage for the Phase 11B fixes — see roadmap_unified.md §11B
// and checkpoint_findings.md. These tests pin behaviour that was either
// silently wrong or DOS-prone pre-fix and would be tricky to retro-detect.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// 11B.11 (H11) — DLQ event must carry a fresh ID (not the original's) so
// downstream dedupers don't drop it.
func TestPhase11B_DLQGetsFreshIDAndOriginalIDHeader(t *testing.T) {
	t.Parallel()
	pub := &capturingPublisher{}
	sub := events.Subscription{
		MaxRetry: 0,
		DLQ:      "myapp.dead",
		Handler: func(_ context.Context, _ events.Event) error {
			return errors.New("nope")
		},
	}
	original := events.Event{
		ID:      "orig-123",
		Type:    "myapp.thing",
		Headers: map[string]string{"trace": "abc"},
	}
	events.DeliverWithRetry(context.Background(), pub, sub, original)

	if len(pub.events) != 1 {
		t.Fatalf("want 1 DLQ event published, got %d", len(pub.events))
	}
	dlq := pub.events[0]
	if dlq.ID == original.ID || dlq.ID == "" {
		t.Errorf("DLQ ID must be fresh and non-empty (got %q, original was %q)", dlq.ID, original.ID)
	}
	if dlq.Type != "myapp.dead" {
		t.Errorf("DLQ type: got %q, want myapp.dead", dlq.Type)
	}
	if dlq.Headers["original_id"] != "orig-123" {
		t.Errorf("DLQ headers.original_id: got %q, want orig-123", dlq.Headers["original_id"])
	}
	if dlq.Headers["original_type"] != "myapp.thing" {
		t.Errorf("DLQ headers.original_type: got %q, want myapp.thing", dlq.Headers["original_type"])
	}
}

// 11B.11 — the retry sleep must abort when ctx is cancelled. Pre-fix the
// loop slept regardless of cancellation, keeping handler goroutines alive
// well past shutdown.
func TestPhase11B_DeliverWithRetry_RespectsContextCancellation(t *testing.T) {
	t.Parallel()
	sub := events.Subscription{
		MaxRetry: 5,
		Backoff:  func(_ int) time.Duration { return time.Hour }, // would block forever
		Handler:  func(_ context.Context, _ events.Event) error { return errors.New("nope") },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		events.DeliverWithRetry(ctx, &capturingPublisher{}, sub, events.Event{ID: "x"})
		close(done)
	}()

	select {
	case <-done:
		// Good — returned promptly despite the 1h backoff.
	case <-time.After(2 * time.Second):
		t.Fatal("DeliverWithRetry blocked despite cancelled ctx (regression of H11)")
	}
}

// 11B.12 (H12) — tokens whose nbf claim is in the future must be rejected.
func TestPhase11B_JWT_NotBeforeRejected(t *testing.T) {
	t.Parallel()
	const secret = "phase11b-secret"
	tok := makeJWTClaims(t, secret, map[string]any{
		"sub": "user-1",
		"nbf": time.Now().Add(10 * time.Minute).Unix(), // future
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(secret))
		},
	})

	resp := srv.GET("/users", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	resp.AssertStatus(http.StatusUnauthorized)
	if got := resp.ErrorCode(); got != "TOKEN_NOT_YET_VALID" {
		t.Errorf("error code: got %q, want TOKEN_NOT_YET_VALID", got)
	}
}

// 11B.12 — tokens whose iat is in the future are also rejected.
func TestPhase11B_JWT_IssuedInFutureRejected(t *testing.T) {
	t.Parallel()
	const secret = "phase11b-secret"
	tok := makeJWTClaims(t, secret, map[string]any{
		"sub": "user-1",
		"iat": time.Now().Add(10 * time.Minute).Unix(), // future
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(secret))
		},
	})

	resp := srv.GET("/users", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	resp.AssertStatus(http.StatusUnauthorized)
	if got := resp.ErrorCode(); got != "TOKEN_FUTURE_ISSUED" {
		t.Errorf("error code: got %q, want TOKEN_FUTURE_ISSUED", got)
	}
}

// 11B.12 — a token whose nbf is already past must pass (sanity check).
func TestPhase11B_JWT_PastNotBeforeAccepted(t *testing.T) {
	t.Parallel()
	const secret = "phase11b-secret"
	tok := makeJWTClaims(t, secret, map[string]any{
		"sub": "user-1",
		"nbf": time.Now().Add(-time.Hour).Unix(),
		"iat": time.Now().Add(-time.Hour).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(secret))
		},
	})

	srv.GET("/users", map[string]string{
		"Authorization": "Bearer " + tok,
	}).AssertStatus(http.StatusOK)
}

// capturingPublisher records every Publish call. Test-only.
type capturingPublisher struct {
	events []events.Event
}

func (p *capturingPublisher) Publish(_ context.Context, e events.Event) error {
	p.events = append(p.events, e)
	return nil
}

func (p *capturingPublisher) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *capturingPublisher) Close() error { return nil }
