package e2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/realtime"
)

// Audit RT-6 (High): the realtime hub forwards an event's Data verbatim, so any
// secret in the payload reaches WS/SSE subscribers and the ResumeStore replay.
// EV-1 closed the source — events.Emit now marshals maniflex.RedactRecord rather
// than the raw decrypted ctx.DBResult — but that was verified at the bus, one
// hop upstream of the hub. These tests carry the same EvtSecret fixtures the
// rest of the way, to the byte a client actually receives, which the existing
// realtime_test.go stack (a User model with a password nobody asserts on) never
// checked. They also cover what the hub adds on its own: the ResumeStore replay
// and the Visibility hook.
//
//	go test ./tests/e2e/... -run TestRealtimeRedaction

// newSecretRealtimeStack wires EvtSecret (one encrypted, one write-only, one
// hidden field) through events.Emit → inproc bus → realtime hub, mounted beside
// the REST API, so a create over HTTP is delivered to a live WS/SSE client.
func newSecretRealtimeStack(t *testing.T, hubCfg realtime.HubConfig) *realtimeStack {
	t.Helper()
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)

	bus := inproc.New()
	hubCfg.Bus = bus
	hub, err := realtime.NewHub(hubCfg)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	srv := maniflex.New(maniflex.Config{
		PathPrefix:  "/api",
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
	})
	srv.MustRegister(EvtSecret{})

	db, err := sqlite.Open(":memory:", srv.Registry())
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.AutoMigrate(context.Background(), srv.Registry()); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	srv.SetDB(db)

	srv.Pipeline.DB.Register(
		events.Emit(bus, events.EmitConfig{}),
		maniflex.AtPosition(maniflex.After),
		maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
	)

	r := chi.NewRouter()
	maniflex.Mount(r, srv)
	r.Handle("/ws", hub.Handler())
	r.Handle("/sse", hub.SSEHandler())

	ts := httptest.NewServer(r)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hub.Shutdown(ctx)
		ts.Close()
		db.Close()
	})

	return &realtimeStack{ts: ts, bus: bus, hub: hub}
}

// createSecret POSTs one EvtSecret carrying all three secret values.
func createSecret(t *testing.T, stack *realtimeStack, title string) string {
	t.Helper()
	return apiPost(t, stack.ts, "/api/evt_secrets", map[string]any{
		"title":    title,
		"ssn":      evtSecretSSN,
		"password": evtSecretPassword,
		"internal": evtSecretInternal,
	})
}

func TestRealtimeRedaction_WebSocketDeliveryStripsSecrets(t *testing.T) {
	stack := newSecretRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "evtSecret.*")

	createSecret(t, stack, "Confidential")

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for evt_secret.created over WebSocket")
	}
	raw, _ := json.Marshal(msg)
	assertNoSecrets(t, "ws delivery", string(raw))

	// Anti-vacuity: a fix that delivered an empty payload would pass every
	// secret check. The record still has to reach the client.
	if !strings.Contains(string(raw), "Confidential") {
		t.Errorf("ws delivery lost the title — redaction over-reached\npayload: %s", raw)
	}
}

func TestRealtimeRedaction_SSEDeliveryStripsSecrets(t *testing.T) {
	stack := newSecretRealtimeStack(t, realtime.HubConfig{})

	c := e2eDialSSE(t, stack.ts, "/sse?subscribe=evtSecret.*")

	createSecret(t, stack, "Confidential")

	msg, ok := c.recvSSEEvent(3 * time.Second)
	if !ok {
		t.Fatal("timed out waiting for evt_secret.created over SSE")
	}
	raw, _ := json.Marshal(msg)
	assertNoSecrets(t, "sse delivery", string(raw))
	if !strings.Contains(string(raw), "Confidential") {
		t.Errorf("sse delivery lost the title — redaction over-reached\npayload: %s", raw)
	}
}

// TestRealtimeRedaction_ResumeReplayStripsSecrets covers the path the audit
// calls out specifically: fanout stamps each event into the ResumeStore, so a
// replay must not resurrect a secret the live delivery had stripped.
func TestRealtimeRedaction_ResumeReplayStripsSecrets(t *testing.T) {
	stack := newSecretRealtimeStack(t, realtime.HubConfig{ResumeBuffer: 64})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "evtSecret.*")

	// First record: capture the cursor the hub stamps on the live delivery.
	createSecret(t, stack, "First")
	first, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for the first event")
	}
	cursor, _ := first["cursor"].(string)
	if cursor == "" {
		t.Fatal("no cursor on the delivered event — resume is not wired, the replay path is untested")
	}

	// Second record, consumed live so only the replay below re-delivers it.
	createSecret(t, stack, "Second")
	if _, ok := e2eRecvTimeout(t, c, 3*time.Second); !ok {
		t.Fatal("timed out waiting for the second event")
	}

	// Resume from the first cursor on a new subscription: the hub replays the
	// second event out of the ResumeStore. That replayed payload is what must
	// be clean.
	b, _ := json.Marshal(map[string]any{
		"op":       "subscribe",
		"patterns": []string{"evtSecret.*"},
		"after":    cursor,
	})
	c.e2eSendFrame(0x1, b)

	replay, ok := recvEventOp(t, c, 3*time.Second)
	if !ok {
		t.Fatal("no replayed event after resuming from the cursor")
	}
	raw, _ := json.Marshal(replay)
	assertNoSecrets(t, "ws resume replay", string(raw))
	if !strings.Contains(string(raw), "Second") {
		t.Errorf("resume replayed the wrong record or dropped its body\npayload: %s", raw)
	}
}

// TestRealtimeRedaction_VisibilityTransformStillRedacted proves the Visibility
// hook is handed an already-redacted event: a transform that returns a copy of
// what it received must still deliver clean data, so the hook cannot become a
// reintroduction point for secrets it was never given.
func TestRealtimeRedaction_VisibilityTransformStillRedacted(t *testing.T) {
	stack := newSecretRealtimeStack(t, realtime.HubConfig{
		Visibility: func(_ *realtime.Principal, e events.Event) (bool, *events.Event) {
			cp := e // exercise the transformed != nil branch
			return true, &cp
		},
	})

	c := e2eDialWS(t, stack.ts, "/ws")
	e2eSubscribe(t, c, "evtSecret.*")

	createSecret(t, stack, "Confidential")

	msg, ok := e2eRecvTimeout(t, c, 3*time.Second)
	if !ok {
		t.Fatal("timed out waiting for the transformed event")
	}
	raw, _ := json.Marshal(msg)
	assertNoSecrets(t, "ws visibility transform", string(raw))
	if !strings.Contains(string(raw), "Confidential") {
		t.Errorf("transform path lost the title\npayload: %s", raw)
	}
}

// recvEventOp reads WS frames until one with op=="event" arrives (skipping the
// subscribe ack), or d elapses.
func recvEventOp(t *testing.T, c *e2eWsConn, d time.Duration) (map[string]any, bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		msg, ok := e2eRecvTimeout(t, c, time.Until(deadline))
		if !ok {
			return nil, false
		}
		if op, _ := msg["op"].(string); op == "event" {
			return msg, true
		}
	}
	return nil, false
}
