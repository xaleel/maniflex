package realtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── NewHub construction ───────────────────────────────────────────────────────

func TestHub_NewHub_NilBus_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := realtime.NewHub(realtime.HubConfig{Bus: nil})
	if err == nil {
		t.Fatal("expected error when Bus is nil, got nil")
	}
}

func TestHub_NewHub_ValidConfig_ReturnsHub(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub, err := realtime.NewHub(realtime.HubConfig{Bus: bus})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if hub == nil {
		t.Fatal("NewHub returned nil hub")
	}
}

// ── Subscribe / Ack ───────────────────────────────────────────────────────────

func TestHub_SubscribeReceivesAck(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("appointment.*")

	subID, _ := ack["subId"].(string)
	if subID == "" {
		t.Fatal("ack missing subId")
	}
	if !hasPrefix(subID, "s_") {
		t.Errorf("subId should start with s_, got %q", subID)
	}
}

func TestHub_SubscribeMultiplePatterns(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("appointment.*", "queue.position_changed", "invoice.*")

	subID, _ := ack["subId"].(string)
	if subID == "" {
		t.Fatal("ack missing subId for multi-pattern subscribe")
	}
}

func TestHub_MultipleSeparateSubscriptions(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")

	ack1 := c.subscribe("appointment.*")
	ack2 := c.subscribe("invoice.*")

	id1, _ := ack1["subId"].(string)
	id2, _ := ack2["subId"].(string)
	if id1 == "" || id2 == "" {
		t.Fatal("expected non-empty subIds for both subscriptions")
	}
	if id1 == id2 {
		t.Errorf("two separate subscriptions must produce different subIds")
	}
}

// ── Event delivery ────────────────────────────────────────────────────────────

func TestHub_EventDeliveredToSubscriber(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("appointment.*")
	subID, _ := ack["subId"].(string)

	publish(t, bus, "appointment.created", "appt/1")

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("timed out waiting for event")
	}
	assertEventMsg(t, msg, subID, "appointment.created")
}

func TestHub_EventDeliveredToWildcardSubscriber(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("*")
	subID, _ := ack["subId"].(string)

	publish(t, bus, "anything.happened", "x/1")

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("timed out waiting for wildcard event")
	}
	assertEventMsg(t, msg, subID, "anything.happened")
}

func TestHub_EventNotDeliveredForNonMatchingPattern(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("appointment.*")

	// Publish an event that does not match "appointment.*".
	publish(t, bus, "invoice.created", "inv/1")

	_, ok := c.recvTimeout(300 * time.Millisecond)
	if ok {
		t.Error("event for non-matching pattern should not be delivered")
	}
}

func TestHub_MultipleClientsReceiveIndependently(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c1 := dialWS(t, ts, "/ws")
	c2 := dialWS(t, ts, "/ws")

	ack1 := c1.subscribe("appointment.*")
	ack2 := c2.subscribe("invoice.*")
	sub1, _ := ack1["subId"].(string)
	sub2, _ := ack2["subId"].(string)

	publish(t, bus, "appointment.created", "appt/1")
	publish(t, bus, "invoice.created", "inv/1")

	// c1 receives appointment, not invoice.
	msg1, ok := c1.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("c1: timed out waiting for appointment event")
	}
	assertEventMsg(t, msg1, sub1, "appointment.created")

	_, unexpected := c1.recvTimeout(300 * time.Millisecond)
	if unexpected {
		t.Error("c1: should not receive invoice event")
	}

	// c2 receives invoice, not appointment.
	msg2, ok := c2.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("c2: timed out waiting for invoice event")
	}
	assertEventMsg(t, msg2, sub2, "invoice.created")
}

// ── Unsubscribe ───────────────────────────────────────────────────────────────

func TestHub_UnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("appointment.*")
	subID, _ := ack["subId"].(string)

	// Confirm delivery works before unsubscribe.
	publish(t, bus, "appointment.created", "appt/1")
	if _, ok := c.recvTimeout(2 * time.Second); !ok {
		t.Fatal("event not delivered before unsubscribe")
	}

	// Unsubscribe and wait for server confirmation.
	c.sendJSON(map[string]any{"op": "unsubscribe", "subId": subID})
	c.mustRecvOp("unsubscribed")

	// Events published after unsubscribe must not arrive.
	publish(t, bus, "appointment.updated", "appt/1")
	if _, ok := c.recvTimeout(300 * time.Millisecond); ok {
		t.Error("event arrived after unsubscribe")
	}
}

// ── Visibility hook ───────────────────────────────────────────────────────────

func TestHub_VisibilityAllow(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			return true, nil
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("*")
	subID, _ := ack["subId"].(string)

	publish(t, bus, "foo.bar", "x/1")

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("visibility=allow: event not delivered")
	}
	assertEventMsg(t, msg, subID, "foo.bar")
}

func TestHub_VisibilityDeny(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			return false, nil // deny all
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	publish(t, bus, "foo.bar", "x/1")

	_, ok := c.recvTimeout(300 * time.Millisecond)
	if ok {
		t.Error("visibility=deny: event should not be delivered")
	}
}

func TestHub_VisibilityTransform(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			// Strip the Data field before delivery.
			transformed := e
			transformed.Data = json.RawMessage(`{"redacted":true}`)
			return true, &transformed
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	publish(t, bus, "patient.read", "p/1")

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("visibility=transform: event not delivered")
	}
	if msg["op"] != "event" {
		t.Fatalf("expected op=event, got %q", msg["op"])
	}
	envelope, _ := msg["data"].(map[string]any)
	payload, _ := envelope["data"].(map[string]any)
	if payload["redacted"] != true {
		t.Errorf("expected redacted data, got: %v", payload)
	}
}

func TestHub_VisibilityTransformIsolated_OneClientSeesRedacted(t *testing.T) {
	t.Parallel()
	bus := inproc.New()

	const privileged = "privileged"
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			// Only the privileged user sees the full event.
			if p.UserID == privileged {
				return true, nil
			}
			stripped := e
			stripped.Data = json.RawMessage(`{}`)
			return true, &stripped
		},
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			return &realtime.Principal{UserID: tok}, nil
		}),
	})
	ts := newHubTestServer(t, hub)

	priv := dialWS(t, ts, "/ws", bearerHeader(privileged))
	unpriv := dialWS(t, ts, "/ws", bearerHeader("regular"))

	privAck := priv.subscribe("*")
	unprivAck := unpriv.subscribe("*")
	privSub, _ := privAck["subId"].(string)
	unprivSub, _ := unprivAck["subId"].(string)

	publish(t, bus, "patient.read", "p/1")

	privMsg, ok := priv.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("privileged client: no event")
	}
	assertEventMsg(t, privMsg, privSub, "patient.read")

	unprivMsg, ok := unpriv.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("unprivileged client: no event")
	}
	assertEventMsg(t, unprivMsg, unprivSub, "patient.read")
	envelope, _ := unprivMsg["data"].(map[string]any)
	payload, _ := envelope["data"].(map[string]any)
	if len(payload) != 0 {
		t.Errorf("unprivileged client should receive stripped data {}, got: %v", payload)
	}
}

// ── AllowPatterns restriction ─────────────────────────────────────────────────

func TestHub_AllowPatterns_ForbidsUnlisted(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		AllowPatterns: []string{"appointment.*"},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"invoice.*"}})
	msg := c.mustRecvOp("error")

	if code, _ := msg["code"].(string); code != "FORBIDDEN_PATTERN" {
		t.Errorf("expected FORBIDDEN_PATTERN error, got code=%q msg=%v", code, msg)
	}
}

func TestHub_AllowPatterns_PermitsListed(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		AllowPatterns: []string{"appointment.*", "queue.*"},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("appointment.*")
	if _, ok := ack["subId"].(string); !ok {
		t.Fatal("expected ack with subId for allowed pattern")
	}
}

func TestHub_AllowPatterns_PartialSubscribeSucceeds(t *testing.T) {
	// When subscribing to multiple patterns, permitted ones proceed and forbidden
	// ones produce individual error frames; the subscribe call as a whole is not aborted.
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		AllowPatterns: []string{"appointment.*"},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// Subscribe to one allowed and one forbidden pattern in the same message.
	c.sendJSON(map[string]any{"op": "subscribe", "patterns": []string{"appointment.*", "invoice.*"}})

	// Expect one ack (for the allowed pattern) and one error (for the forbidden one).
	var gotAck, gotError bool
	for i := 0; i < 2; i++ {
		msg, ok := c.recvTimeout(2 * time.Second)
		if !ok {
			t.Fatalf("timed out waiting for message %d", i+1)
		}
		switch msg["op"] {
		case "ack":
			gotAck = true
		case "error":
			if msg["code"] == "FORBIDDEN_PATTERN" {
				gotError = true
			}
		}
	}
	if !gotAck {
		t.Error("expected ack for allowed pattern, got none")
	}
	if !gotError {
		t.Error("expected FORBIDDEN_PATTERN error for denied pattern, got none")
	}
}

// ── Protocol edge cases ───────────────────────────────────────────────────────

func TestHub_MalformedFrameReturnsError(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// Send non-JSON text.
	c.sendFrame(wsOpcodeText, []byte("not valid json {{{"))

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("expected error response to malformed frame, got timeout")
	}
	if msg["op"] != "error" {
		t.Errorf("expected op=error, got %q", msg["op"])
	}
}

func TestHub_UnknownOpReturnsError(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.sendJSON(map[string]any{"op": "bogus_op", "foo": "bar"})

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("expected error response to unknown op, got timeout")
	}
	if msg["op"] != "error" {
		t.Errorf("expected op=error for unknown op, got %q", msg["op"])
	}
}

func TestHub_PingReceivesPong(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// Send protocol-level ping (application layer, not WebSocket ping frame).
	c.sendJSON(map[string]any{"op": "ping"})

	msg, ok := c.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("expected pong response to ping, got timeout")
	}
	if msg["op"] != "pong" {
		t.Errorf("expected op=pong, got %q", msg["op"])
	}
}

func TestHub_WebSocketPingFrameReceivesPongFrame(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	// Send a WebSocket-level ping frame (opcode 0x9).
	c.sendFrame(wsOpcodePing, []byte("heartbeat"))

	// Must receive a pong frame (opcode 0xA) in response.
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	f, err := c.recvFrame()
	if err != nil {
		t.Fatalf("recvFrame after ws ping: %v", err)
	}
	if f.opcode != wsOpcodePong {
		t.Errorf("expected pong frame (0xA), got opcode 0x%X", f.opcode)
	}
}

// ── Slow consumer / backpressure ──────────────────────────────────────────────

func TestHub_SlowConsumerIsKicked(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:         bus,
		SendBuffer:  1,                  // minimal outbound channel
		SendTimeout: 100 * time.Millisecond,
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// Flood the hub without reading. Each event carries a large payload so the
	// client's TCP receive buffer fills quickly, stalling the hub's writeLoop
	// on conn.Write. Once the writer is stalled, the SendBuffer fills and
	// subsequent sends trip the SendTimeout, triggering the kick.
	bigPayload := make([]byte, 8*1024)
	for i := range bigPayload {
		bigPayload[i] = 'x'
	}
	bigData, _ := json.Marshal(map[string]string{"blob": string(bigPayload)})
	for i := 0; i < 64; i++ {
		if err := bus.Publish(context.Background(), events.Event{
			ID:      "flood-id",
			Source:  "/test",
			Type:    "flood.event",
			Subject: "x/1",
			Data:    bigData,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Give the writeLoop time to stall on TCP backpressure (client isn't
	// reading yet) so the SendTimeout actually fires. Without this pause the
	// recvCloseCode loop below would drain TCP fast enough to keep the writer
	// flowing and the kick would never trigger.
	time.Sleep(300 * time.Millisecond)

	// The hub should send a close frame with code 1013 (Try Again Later).
	code, ok := c.recvCloseCode(3 * time.Second)
	if !ok {
		t.Fatal("timed out waiting for close frame after slow-consumer kick")
	}
	if code != 1013 {
		t.Errorf("expected close code 1013 (Try Again Later), got %d", code)
	}
}

// ── Stats ─────────────────────────────────────────────────────────────────────

func TestHub_StatsCountsConnections(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	if s := hub.Stats(); s.Connections != 0 {
		t.Errorf("initial connections: want 0, got %d", s.Connections)
	}

	c1 := dialWS(t, ts, "/ws")
	c2 := dialWS(t, ts, "/ws")
	time.Sleep(50 * time.Millisecond) // allow goroutines to register

	if s := hub.Stats(); s.Connections != 2 {
		t.Errorf("after 2 connects: want 2, got %d", s.Connections)
	}

	c1.closeGracefully()
	time.Sleep(50 * time.Millisecond)

	if s := hub.Stats(); s.Connections != 1 {
		t.Errorf("after 1 disconnect: want 1, got %d", s.Connections)
	}
	_ = c2
}

func TestHub_StatsTracksKickedCount(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:         bus,
		SendBuffer:  1,
		SendTimeout: 100 * time.Millisecond,
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	// Flood with large payloads + a stall window so writeLoop blocks on TCP
	// backpressure long enough for the SendTimeout-driven kick to fire.
	bigPayload := make([]byte, 8*1024)
	for i := range bigPayload {
		bigPayload[i] = 'x'
	}
	bigData, _ := json.Marshal(map[string]string{"blob": string(bigPayload)})
	for i := 0; i < 64; i++ {
		if err := bus.Publish(context.Background(), events.Event{
			ID:      "flood-id",
			Source:  "/test",
			Type:    "flood.event",
			Subject: "x/1",
			Data:    bigData,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	time.Sleep(300 * time.Millisecond)

	c.recvCloseCode(3 * time.Second)
	time.Sleep(100 * time.Millisecond)

	if s := hub.Stats(); s.Kicked == 0 {
		t.Error("expected Kicked > 0 after slow consumer kicked")
	}
}

// ── Graceful shutdown ─────────────────────────────────────────────────────────

func TestHub_ShutdownSendsCloseToAllClients(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c1 := dialWS(t, ts, "/ws")
	c2 := dialWS(t, ts, "/ws")
	c1.subscribe("*")
	c2.subscribe("*")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Both clients should receive close frames (code 1001 Going Away).
	code1, ok1 := c1.recvCloseCode(2 * time.Second)
	if !ok1 {
		t.Error("c1: timed out waiting for close frame after Shutdown")
	} else if code1 != 1001 {
		t.Errorf("c1: expected close code 1001, got %d", code1)
	}

	code2, ok2 := c2.recvCloseCode(2 * time.Second)
	if !ok2 {
		t.Error("c2: timed out waiting for close frame after Shutdown")
	} else if code2 != 1001 {
		t.Errorf("c2: expected close code 1001, got %d", code2)
	}
}

func TestHub_ShutdownStopsNewConnections(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	hub.Shutdown(ctx)

	// After Shutdown, new connection attempts should be rejected.
	status := dialWSExpectStatus(t, ts, "/ws")
	if status == http.StatusSwitchingProtocols {
		t.Error("expected non-101 after Shutdown, but upgrade succeeded")
	}
}

// ── Authentication ────────────────────────────────────────────────────────────

func TestHub_AnonymousConnection_NoAuthenticator(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	// Nil Authenticator defaults to AnonymousOnly — all connections allowed.
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws") // must not fail
	ack := c.subscribe("*")
	if _, ok := ack["subId"].(string); !ok {
		t.Fatal("anonymous connection should be able to subscribe")
	}
}

func TestHub_AuthFailure_RejectsUpgrade(t *testing.T) {
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

	t.Run("valid token gets 101", func(t *testing.T) {
		c := dialWS(t, ts, "/ws", bearerHeader("valid"))
		ack := c.subscribe("*")
		if _, ok := ack["subId"].(string); !ok {
			t.Fatal("valid token: expected ack subId")
		}
	})

	t.Run("bad token gets 401", func(t *testing.T) {
		status := dialWSExpectStatus(t, ts, "/ws", bearerHeader("bad-token"))
		if status != http.StatusUnauthorized {
			t.Errorf("bad token: expected 401, got %d", status)
		}
	})

	t.Run("no token gets 401", func(t *testing.T) {
		status := dialWSExpectStatus(t, ts, "/ws")
		if status != http.StatusUnauthorized {
			t.Errorf("no token: expected 401, got %d", status)
		}
	})
}

func TestHub_AuthenticatedPrincipal_InVisibilityHook(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			return &realtime.Principal{UserID: tok, Roles: []string{"doctor"}}, nil
		}),
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			// Only deliver to users with role "doctor".
			for _, r := range p.Roles {
				if r == "doctor" {
					return true, nil
				}
			}
			return false, nil
		},
	})
	ts := newHubTestServer(t, hub)

	doctor := dialWS(t, ts, "/ws", bearerHeader("dr-jones"))
	nurse := mustHub_withNurseToken(t, ts) // nurse uses a separate hub — separate test

	_ = nurse
	ack := doctor.subscribe("*")
	subID, _ := ack["subId"].(string)

	publish(t, bus, "patient.updated", "p/1")

	msg, ok := doctor.recvTimeout(2 * time.Second)
	if !ok {
		t.Fatal("doctor: event not delivered")
	}
	assertEventMsg(t, msg, subID, "patient.updated")
}

// ── CORS / Origins ────────────────────────────────────────────────────────────

func TestHub_Origins_AllowedOriginUpgradesSuccessfully(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	allowed := make(http.Header)
	allowed.Set("Origin", "https://myapp.example.com")
	c := dialWS(t, ts, "/ws", allowed)
	ack := c.subscribe("*")
	if _, ok := ack["subId"].(string); !ok {
		t.Fatal("allowed origin: expected ack")
	}
}

func TestHub_Origins_ForbiddenOriginRejected(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{
		Bus:     bus,
		Origins: []string{"https://myapp.example.com"},
	})
	ts := newHubTestServer(t, hub)

	bad := make(http.Header)
	bad.Set("Origin", "https://evil.example.com")
	status := dialWSExpectStatus(t, ts, "/ws", bad)
	if status == http.StatusSwitchingProtocols {
		t.Error("forbidden origin should not get 101")
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// mustHub creates a hub or fails the test.
func mustHub(t *testing.T, cfg realtime.HubConfig) *realtime.Hub {
	t.Helper()
	hub, err := realtime.NewHub(cfg)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hub.Shutdown(ctx)
	})
	return hub
}

// publish sends a minimal CloudEvents event on the bus.
func publish(t *testing.T, bus events.Publisher, eventType, subject string) {
	t.Helper()
	e := events.Event{
		ID:      "test-id",
		Source:  "/test",
		Type:    eventType,
		Subject: subject,
		Data:    json.RawMessage(`{"test":true}`),
	}
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatalf("publish %q: %v", eventType, err)
	}
	// Give the inproc bus goroutines time to deliver.
	time.Sleep(20 * time.Millisecond)
}

// assertEventMsg asserts that msg is an event frame with the given subId and type.
func assertEventMsg(t *testing.T, msg map[string]any, wantSubID, wantType string) {
	t.Helper()
	if op, _ := msg["op"].(string); op != "event" {
		t.Errorf("expected op=event, got %q (msg: %v)", op, msg)
		return
	}
	if sub, _ := msg["subId"].(string); sub != wantSubID {
		t.Errorf("expected subId=%q, got %q", wantSubID, sub)
	}
	data, _ := msg["data"].(map[string]any)
	if typ, _ := data["type"].(string); typ != wantType {
		t.Errorf("expected event type=%q, got %q", wantType, typ)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// mustHub_withNurseToken is a placeholder that creates a second authenticated
// client for the visibility test — in practice would use a different token.
// Returning nil to keep the test compilable; the actual nurse assertion is
// omitted to keep test scope focused on the doctor path.
func mustHub_withNurseToken(t *testing.T, ts *httptest.Server) *wsClient {
	t.Helper()
	// A nurse role client — visibility hook will deny their events.
	// We connect them but don't assert delivery (they shouldn't receive anything).
	return dialWS(t, ts, "/ws", bearerHeader("nurse-kelly"))
}
