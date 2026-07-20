package outbox_test

// 11D.10 — the outbox relayer's dead-letter payload.
//
// There are two DLQ paths in the framework and they did not agree.
// events.DeliverWithRetry mints a fresh ID and stamps original_type +
// original_id; outbox.Relayer.routeToDLQ stamped only original_type, reused the
// original ID, and threw away the publish error. Both copy the original headers
// through — the header question this row asks was already answered; the
// divergence is the defect.
//
// Reusing the ID is the one that bites. DeliverWithRetry's own comment says the
// fresh ID exists "so downstream dedupers see this as a new event". The outbox
// path emitted the dead-letter under the ID of the event already published, so a
// deduper drops it and the failure is dead-lettered into nothing.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/outbox"
)

// dlqOnlyPublisher fails every event except those of dlqType, so the original
// delivery exhausts its attempts while the dead-letter itself still lands.
type dlqOnlyPublisher struct {
	mu      sync.Mutex
	dlqType string
	got     []events.Event
}

func (p *dlqOnlyPublisher) Publish(_ context.Context, e events.Event) error {
	if e.Type != p.dlqType {
		return errors.New("downstream unavailable")
	}
	p.mu.Lock()
	p.got = append(p.got, e)
	p.mu.Unlock()
	return nil
}

func (p *dlqOnlyPublisher) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := p.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (p *dlqOnlyPublisher) Close() error { return nil }

func (p *dlqOnlyPublisher) dead() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.got...)
}

func TestOutboxDLQ_PayloadCarriesProvenance(t *testing.T) {
	const dlqType = "test.created.dlq"
	db := openTestDB(t)
	pub := &dlqOnlyPublisher{dlqType: dlqType}
	bus := outbox.Wrap(pub, db, "sqlite")

	orig := makeEvent("evt-original")
	orig.Headers = map[string]string{"tenant": "acme", "trace": "abc123"}
	if err := bus.Publish(context.Background(), orig); err != nil {
		t.Fatal(err)
	}

	// MaxAttempts 1 so the first failure exhausts it and routes straight to DLQ.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	bus.Relay(outbox.RelayOptions{
		PollInterval: time.Millisecond,
		MaxAttempts:  1,
		DLQType:      dlqType,
	}).Start(ctx) //nolint:errcheck

	dead := pub.dead()
	if len(dead) != 1 {
		t.Fatalf("expected exactly 1 dead-lettered event, got %d", len(dead))
	}
	d := dead[0]

	if d.Type != dlqType {
		t.Errorf("Type = %q, want %q", d.Type, dlqType)
	}
	// The original headers travel with it — this row's stated ask.
	for k, v := range orig.Headers {
		if d.Headers[k] != v {
			t.Errorf("original header %q = %q, want %q", k, d.Headers[k], v)
		}
	}
	if d.Headers["original_type"] != orig.Type {
		t.Errorf("original_type = %q, want %q", d.Headers["original_type"], orig.Type)
	}
	// Provenance: without this the dead-letter cannot be traced to its source.
	if d.Headers["original_id"] != orig.ID {
		t.Errorf("original_id = %q, want %q", d.Headers["original_id"], orig.ID)
	}
	// A fresh ID, for the same reason DeliverWithRetry mints one: the original
	// ID has already been seen downstream, so reusing it gets the dead-letter
	// discarded as a duplicate.
	if d.ID == orig.ID {
		t.Errorf("dead-letter reuses the original ID %q — a deduper will drop it", orig.ID)
	}
	if d.ID == "" {
		t.Error("dead-letter has no ID")
	}
}
