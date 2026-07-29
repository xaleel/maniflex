package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/events"
)

// synchronousTestBus makes publication complete only after the hub subscriber
// has handled the event. That lets the test place an event exactly at the first
// response flush instead of relying on scheduler timing to expose the gap.
type synchronousTestBus struct {
	sub events.Subscription
}

func (b *synchronousTestBus) Publish(ctx context.Context, e events.Event) error {
	return b.sub.Handler(ctx, e)
}

func (b *synchronousTestBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		if err := b.Publish(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (b *synchronousTestBus) Subscribe(_ context.Context, sub events.Subscription) (events.Cancel, error) {
	b.sub = sub
	return func() {}, nil
}

func (b *synchronousTestBus) Close() error { return nil }

type publishOnFlushWriter struct {
	header  http.Header
	flush   sync.Once
	onFlush func()
	writes  chan []byte
}

func (w *publishOnFlushWriter) Header() http.Header {
	return w.header
}

func (w *publishOnFlushWriter) WriteHeader(int) {}

func (w *publishOnFlushWriter) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	select {
	case w.writes <- cp:
	default:
	}
	return len(p), nil
}

func (w *publishOnFlushWriter) Flush() {
	w.flush.Do(w.onFlush)
}

func TestSSE_ClientRegisteredBeforeResponseFlush(t *testing.T) {
	bus := &synchronousTestBus{}
	hub, err := NewHub(HubConfig{Bus: bus})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})

	published := make(chan error, 1)
	w := &publishOnFlushWriter{
		header: make(http.Header),
		writes: make(chan []byte, 1),
		onFlush: func() {
			published <- bus.Publish(context.Background(), events.Event{
				ID:   "flush-event",
				Type: "appointment.created",
			})
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodGet, "/sse?subscribe=appointment.*", nil).WithContext(ctx)
	handlerDone := make(chan struct{})
	go func() {
		hub.SSEHandler().ServeHTTP(w, req)
		close(handlerDone)
	}()

	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("publish during initial flush: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SSE handler never flushed its response")
	}

	select {
	case frame := <-w.writes:
		if !strings.Contains(string(frame), `"type":"appointment.created"`) {
			t.Fatalf("unexpected SSE frame: %q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("event published during initial flush was lost")
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not stop after request cancellation")
	}
}
