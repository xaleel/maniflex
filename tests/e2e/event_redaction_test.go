package e2e_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit EV-1 (Critical): events.Emit marshalled the raw ctx.DBResult — the
// DB-column map the DB step has already *decrypted* — straight into the event
// Data, bypassing the response serializer that strips hidden and write-only
// fields. The payload flows to every subscriber, every broker topic, the
// persisted event_outbox.payload row, and on through the realtime hub to WS/SSE
// clients (audit RT-6, which is the same leak one hop downstream).
//
// Same root cause as MS-3 (versioning snapshots), fixed with the same exclusion
// set: maniflex.RedactRecord.
//
//	go test ./tests/e2e/... -run TestEventRedaction

// EvtSecret carries one field of each kind the event payload must not record in
// the clear, mirroring HistSecret in history_redaction_test.go.
type EvtSecret struct {
	maniflex.BaseModel
	Title    string `json:"title"    db:"title"`
	SSN      string `json:"ssn"      db:"ssn"      mfx:"encrypted"`
	Password string `json:"password" db:"password" mfx:"writeonly"`
	Internal string `json:"internal" db:"internal" mfx:"hidden,writeonly"`
}

// The secret values, distinct so a leak names which field leaked.
const (
	evtSecretSSN      = "EVT-SSN-PLAINTEXT-111"
	evtSecretPassword = "EVT-PASSWORD-PLAINTEXT-222"
	evtSecretInternal = "EVT-INTERNAL-PLAINTEXT-333"
	evtSecretNewSSN   = "EVT-SSN-PLAINTEXT-444"
)

// collectBus records the raw Data bytes of every event published.
type collectBus struct {
	mu  sync.Mutex
	got []events.Event
}

func (b *collectBus) Publish(_ context.Context, e events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, e)
	return nil
}

func (b *collectBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (b *collectBus) Close() error { return nil }

// payloads waits for at least n events and returns their Data as strings.
// Emit publishes fire-and-forget in a goroutine, so this polls rather than
// reading once.
func (b *collectBus) payloads(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		if len(b.got) >= n {
			out := make([]string, len(b.got))
			for i, e := range b.got {
				out[i] = string(e.Data)
			}
			b.mu.Unlock()
			return out
		}
		count := len(b.got)
		b.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d events, got %d within 2s", n, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func evtSecretServer(t *testing.T, bus events.Publisher) *testutil.Server {
	t.Helper()
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{EvtSecret{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				events.Emit(bus),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
				maniflex.AtPosition(maniflex.After))
		},
	})
}

// assertNoSecrets fails naming the specific field that leaked, so a partial fix
// cannot pass by closing only one of the three.
func assertNoSecrets(t *testing.T, label, payload string) {
	t.Helper()
	for _, c := range []struct{ name, val string }{
		{"encrypted ssn", evtSecretSSN},
		{"encrypted ssn (updated)", evtSecretNewSSN},
		{"writeonly password", evtSecretPassword},
		{"hidden+writeonly internal", evtSecretInternal},
	} {
		if strings.Contains(payload, c.val) {
			t.Errorf("%s: %s leaked into the event payload in plaintext\npayload: %s",
				label, c.name, payload)
		}
	}
	// The HMAC companion of an encrypted+unique column is a searchable digest of
	// the plaintext; it has no business on the wire either.
	if strings.Contains(payload, "ssn_hmac") {
		t.Errorf("%s: ssn_hmac companion column leaked into the event payload\npayload: %s",
			label, payload)
	}
}

func TestEventRedaction_CreateAndUpdatePayloads(t *testing.T) {
	bus := &collectBus{}
	srv := evtSecretServer(t, bus)

	id := srv.MustID(srv.POST("/evt_secrets", map[string]any{
		"title":    "Confidential",
		"ssn":      evtSecretSSN,
		"password": evtSecretPassword,
		"internal": evtSecretInternal,
	}))

	srv.PATCH("/evt_secrets/"+id, map[string]any{"ssn": evtSecretNewSSN})

	got := bus.payloads(t, 2)
	assertNoSecrets(t, "create", got[0])
	assertNoSecrets(t, "update", got[1])
}

// Anti-vacuity: a fix that redacted everything — or emitted an empty payload —
// would pass every assertion above. The event still has to carry the record.
func TestEventRedaction_OrdinaryFieldsSurvive(t *testing.T) {
	bus := &collectBus{}
	srv := evtSecretServer(t, bus)

	id := srv.MustID(srv.POST("/evt_secrets", map[string]any{
		"title":    "Confidential",
		"ssn":      evtSecretSSN,
		"password": evtSecretPassword,
		"internal": evtSecretInternal,
	}))

	payload := bus.payloads(t, 1)[0]
	for _, want := range []string{`"title"`, "Confidential", id} {
		if !strings.Contains(payload, want) {
			t.Errorf("event payload lost %q — redaction over-reached\npayload: %s", want, payload)
		}
	}
}
