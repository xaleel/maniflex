package realtime_test

import (
	"testing"
	"time"

	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/realtime"
)

// ── Inbound frame validation (RT-8) ───────────────────────────────────────────
//
// recvFrame took opcode = hdr[0]&0x0f and ignored the FIN and RSV bits, and it
// read the mask bit without requiring it — so an unmasked frame was silently
// accepted (RFC 6455 §5.1 requires the server to close on one) and a fragmented
// message was mis-parsed (the first fragment dispatched as if whole, the
// continuation frames falling through the switch and vanishing). The hub now
// requires masked, single-frame messages and answers any violation with a 1002
// (Protocol Error) close. These tests drive raw bytes, since the standard test
// client always sends well-formed masked frames.

// rawMaskedFrame builds a client frame with b0 as the full first byte
// (FIN|RSV|opcode), applying the client masking the protocol requires. Payloads
// over 125 bytes use the 16-bit extended length form.
func rawMaskedFrame(b0 byte, payload []byte) []byte {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	n := len(payload)
	var hdr []byte
	if n <= 125 {
		hdr = []byte{b0, byte(0x80 | n)}
	} else {
		hdr = []byte{b0, 0x80 | 126, byte(n >> 8), byte(n)}
	}
	hdr = append(hdr, mask[:]...)
	m := make([]byte, n)
	for i, c := range payload {
		m[i] = c ^ mask[i%4]
	}
	return append(hdr, m...)
}

// rawUnmaskedFrame builds a client frame WITHOUT the mask bit — illegal from a
// client, which is the point.
func rawUnmaskedFrame(b0 byte, payload []byte) []byte {
	n := len(payload)
	hdr := []byte{b0, byte(n)} // n ≤ 125 for these tests; no mask bit
	return append(hdr, payload...)
}

// assertProtocolClose sends raw and asserts the server answers with close 1002.
func assertProtocolClose(t *testing.T, label string, raw []byte) {
	t.Helper()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	if _, err := c.conn.Write(raw); err != nil {
		t.Fatalf("%s: write raw frame: %v", label, err)
	}
	code, ok := c.recvCloseCode(2 * time.Second)
	if !ok {
		t.Fatalf("%s: no close frame — the malformed frame was accepted instead of refused", label)
	}
	if code != 1002 {
		t.Errorf("%s: close code: want 1002 (protocol error), got %d", label, code)
	}
}

func TestWSFrame_RejectsUnmaskedFrame(t *testing.T) {
	// 0x81 = FIN + text, no mask bit.
	assertProtocolClose(t, "unmasked", rawUnmaskedFrame(0x81, []byte(`{"op":"ping"}`)))
}

func TestWSFrame_RejectsFragmentStart(t *testing.T) {
	// 0x01 = text with FIN=0 (a fragment start).
	assertProtocolClose(t, "fragment start", rawMaskedFrame(0x01, []byte(`{"op":"ping"}`)))
}

func TestWSFrame_RejectsContinuationOpcode(t *testing.T) {
	// 0x80 = FIN + continuation opcode (0x0), with no message in progress.
	assertProtocolClose(t, "continuation", rawMaskedFrame(0x80, []byte("more")))
}

func TestWSFrame_RejectsReservedOpcode(t *testing.T) {
	// 0x83 = FIN + reserved data opcode 0x3.
	assertProtocolClose(t, "reserved opcode", rawMaskedFrame(0x83, []byte("x")))
}

func TestWSFrame_RejectsRSVBit(t *testing.T) {
	// 0xC1 = FIN + RSV1 + text, with no extension negotiated.
	assertProtocolClose(t, "rsv1", rawMaskedFrame(0xC1, []byte(`{"op":"ping"}`)))
}

func TestWSFrame_RejectsOversizedControlFrame(t *testing.T) {
	// 0x89 = FIN + ping, but a 200-byte payload forces the extended length form,
	// which a control frame may never use (payload ≤ 125).
	assertProtocolClose(t, "oversized control", rawMaskedFrame(0x89, make([]byte, 200)))
}

// TestWSFrame_WellFormedStillWorks is the over-reach guard: the new checks must
// not reject an ordinary masked single-frame message.
func TestWSFrame_WellFormedStillWorks(t *testing.T) {
	t.Parallel()
	bus := inproc.New()
	hub := mustHub(t, realtime.HubConfig{Bus: bus})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	ack := c.subscribe("*") // masked, FIN=1 text via the standard client
	if _, ok := ack["subId"].(string); !ok {
		t.Fatal("a well-formed masked frame should be accepted, got no ack")
	}
	// A masked ping control frame still round-trips too.
	c.sendJSON(map[string]any{"op": "ping"})
	if got := c.mustRecvOp("pong"); got == nil {
		t.Fatal("masked control message not handled")
	}
}
