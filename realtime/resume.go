package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"

	"github.com/xaleel/maniflex/events"
)

// BufferedEvent pairs a fanned-out event with the cursor the hub stamped on it.
// Replay returns these so the hub can re-emit each event's cursor (SSE id: line
// or WS "cursor" field) and the client's resume position advances correctly.
type BufferedEvent struct {
	Cursor string
	Event  events.Event
}

// ResumeStore buffers recently fanned-out events so a reconnecting client can
// replay everything it missed since its last-seen cursor. Implementations must
// be safe for concurrent use.
//
// The default in-memory implementation (NewMemoryResumeStore) is single-process:
// resume works only when the client reconnects to the same replica (WebSocket
// affinity). A multi-replica deployment can supply its own store (e.g. backed by
// a Redis stream) without any other hub change.
type ResumeStore interface {
	// Append records e under a freshly-minted, monotonically increasing cursor
	// and returns that cursor.
	Append(e events.Event) (cursor string)

	// Replay returns the events recorded strictly after `after`, oldest-first.
	// ok is false when `after` is malformed, belongs to a different store epoch
	// (e.g. the hub restarted), or predates the retained window — the caller
	// must then tell the client to resync rather than assume nothing was missed.
	Replay(after string) (evs []BufferedEvent, ok bool)
}

// memoryResumeStore is an in-process ring buffer of the most recent events.
type memoryResumeStore struct {
	epoch string

	mu   sync.Mutex
	seq  uint64
	cap  int
	ring []storedEvent // oldest → newest, len ≤ cap
}

type storedEvent struct {
	seq uint64
	ev  events.Event
}

// NewMemoryResumeStore returns an in-process ResumeStore that retains the most
// recent `capacity` events for replay. A capacity ≤ 0 falls back to 1024.
func NewMemoryResumeStore(capacity int) ResumeStore {
	if capacity <= 0 {
		capacity = 1024
	}
	return &memoryResumeStore{epoch: newEpoch(), cap: capacity}
}

func (m *memoryResumeStore) Append(e events.Event) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.ring = append(m.ring, storedEvent{seq: m.seq, ev: e})
	if len(m.ring) > m.cap {
		// Drop the oldest entries and re-pack so the backing array can't grow
		// without bound as old slots are shifted out.
		m.ring = append(m.ring[:0:0], m.ring[len(m.ring)-m.cap:]...)
	}
	return formatCursor(m.epoch, m.seq)
}

func (m *memoryResumeStore) Replay(after string) ([]BufferedEvent, bool) {
	epoch, seq, ok := parseCursor(after)
	if !ok || epoch != m.epoch {
		return nil, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.ring) == 0 {
		// Nothing has ever been buffered; the client cannot have missed
		// anything beyond its own cursor.
		return nil, seq >= m.seq
	}

	oldest := m.ring[0].seq
	// A gap exists only if the next event the client needs (seq+1) was already
	// evicted — i.e. the oldest retained event is newer than seq+1.
	if seq+1 < oldest {
		return nil, false
	}

	var out []BufferedEvent
	for _, se := range m.ring {
		if se.seq > seq {
			out = append(out, BufferedEvent{Cursor: formatCursor(m.epoch, se.seq), Event: se.ev})
		}
	}
	return out, true
}

// ── Cursor encoding ────────────────────────────────────────────────────────────

// newEpoch returns an opaque per-store identifier. A reconnecting client that
// presents a cursor from a previous store epoch is told to resync, since the
// new store cannot prove which events it missed.
func newEpoch() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func formatCursor(epoch string, seq uint64) string {
	return epoch + ":" + strconv.FormatUint(seq, 10)
}

func parseCursor(s string) (epoch string, seq uint64, ok bool) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(s[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return s[:i], n, true
}
