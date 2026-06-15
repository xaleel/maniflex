package realtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"maniflex/events"
	"maniflex/events/inproc"
	"maniflex/realtime"
)

// ── SSE basic delivery ────────────────────────────────────────────────────────

func TestSSE_EventDeliveredToSubscriber(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=appointment.*")

	publish(t, bus, "appointment.created", "appt/1")

	msg, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("SSE: timed out waiting for event")
	}
	if typ, _ := msg["type"].(string); typ != "appointment.created" {
		t.Errorf("expected type=appointment.created, got %q", typ)
	}
}

func TestSSE_WildcardPatternReceivesAllEvents(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=*")

	publish(t, bus, "anything.happened", "x/1")

	msg, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("SSE wildcard: timed out waiting for event")
	}
	if typ, _ := msg["type"].(string); typ != "anything.happened" {
		t.Errorf("expected type=anything.happened, got %q", typ)
	}
}

// ── SSE pattern filtering ─────────────────────────────────────────────────────

func TestSSE_EventNotDeliveredForNonMatchingPattern(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=appointment.*")

	publish(t, bus, "invoice.created", "inv/1")

	_, ok := c.recvEvent(300 * time.Millisecond)
	if ok {
		t.Error("SSE: event for non-matching pattern should not be delivered")
	}
}

// ── SSE multiple patterns ─────────────────────────────────────────────────────

func TestSSE_MultiplePatterns(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=appointment.*&subscribe=invoice.*")

	publish(t, bus, "appointment.created", "appt/1")
	msg1, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("SSE multi-pattern: appointment event not received")
	}
	if typ, _ := msg1["type"].(string); typ != "appointment.created" {
		t.Errorf("expected appointment.created, got %q", typ)
	}

	publish(t, bus, "invoice.created", "inv/1")
	msg2, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("SSE multi-pattern: invoice event not received")
	}
	if typ, _ := msg2["type"].(string); typ != "invoice.created" {
		t.Errorf("expected invoice.created, got %q", typ)
	}
}

// ── SSE Content-Type and headers ──────────────────────────────────────────────

func TestSSE_ResponseContentType(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	// dialSSE already asserts Content-Type: text/event-stream.
	// This test verifies the header is present from the server side.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || ct[:17] != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %q", ct)
	}
	// Cache-Control should prevent caching of event streams.
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control: no-cache, got %q", cc)
	}
}

// ── SSE authentication ────────────────────────────────────────────────────────

func TestSSE_AuthFailure_Returns401(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			if tok == "valid" {
				return &realtime.Principal{UserID: "u1"}, nil
			}
			return nil, &realtime.ErrUnauthorized{Reason: "invalid token"}
		}),
	})
	ts := newHubTestServer(t, hub)

	t.Run("no token → 401", func(t *testing.T) {
		status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*")
		if status != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", status)
		}
	})

	t.Run("bad token → 401", func(t *testing.T) {
		status := dialSSEExpectStatus(t, ts, "/sse?subscribe=*", bearerHeader("bad"))
		if status != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", status)
		}
	})

	t.Run("valid token → 200 stream", func(t *testing.T) {
		c := dialSSE(t, ts, "/sse?subscribe=*", bearerHeader("valid"))
		// Publish and verify we receive events.
		publish(t, bus, "test.event", "t/1")
		_, ok := c.recvEvent(2 * time.Second)
		if !ok {
			t.Fatal("valid token: SSE event not received")
		}
	})
}

// ── SSE visibility ────────────────────────────────────────────────────────────

func TestSSE_VisibilityDeny_EventNotDelivered(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			return false, nil
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=*")
	publish(t, bus, "foo.bar", "x/1")

	_, ok := c.recvEvent(300 * time.Millisecond)
	if ok {
		t.Error("SSE: event should be blocked by visibility hook")
	}
}

func TestSSE_VisibilityTransform_DeliveredWithModifiedData(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			modified := e
			modified.Data = json.RawMessage(`{"transformed":true}`)
			return true, &modified
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=*")
	publish(t, bus, "foo.bar", "x/1")

	msg, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("SSE transform: event not delivered")
	}
	// The SSE data line should contain the full CloudEvents event JSON.
	// Check the data field was replaced.
	data, _ := msg["data"].(map[string]any)
	if data["transformed"] != true {
		t.Errorf("expected transformed:true in data, got: %v", data)
	}
}

// ── SSE AllowPatterns restriction ─────────────────────────────────────────────

func TestSSE_AllowPatterns_ForbidsUnlisted(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		AllowPatterns: []string{"appointment.*"},
	})
	ts := newHubTestServer(t, hub)

	// Subscribing to an unlisted pattern via SSE should result in 403.
	status := dialSSEExpectStatus(t, ts, "/sse?subscribe=invoice.*")
	if status != http.StatusForbidden {
		t.Errorf("expected 403 for forbidden SSE pattern, got %d", status)
	}
}

func TestSSE_AllowPatterns_PermitsListed(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		AllowPatterns: []string{"appointment.*"},
	})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=appointment.*")
	publish(t, bus, "appointment.created", "appt/1")

	_, ok := c.recvEvent(2 * time.Second)
	if !ok {
		t.Fatal("allowed SSE pattern: event not received")
	}
}

// ── SSE client disconnect ─────────────────────────────────────────────────────

func TestSSE_ClientDisconnect_HubCleansUp(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=*")
	time.Sleep(50 * time.Millisecond)

	if s := hub.Stats(); s.Connections != 1 {
		t.Errorf("before disconnect: want 1 connection, got %d", s.Connections)
	}

	// Close the response body to simulate client disconnect.
	c.resp.Body.Close()
	time.Sleep(100 * time.Millisecond)

	if s := hub.Stats(); s.Connections != 0 {
		t.Errorf("after disconnect: want 0 connections, got %d", s.Connections)
	}
}

// ── SSE graceful shutdown ─────────────────────────────────────────────────────

func TestSSE_ShutdownClosesStreams(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialSSE(t, ts, "/sse?subscribe=*")
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hub.Shutdown(ctx)

	// After shutdown, reading from the stream should return no more events
	// (the body should be closed or the scanner should stop).
	_, ok := c.recvEvent(500 * time.Millisecond)
	if ok {
		t.Error("SSE: received event after Shutdown")
	}
}
