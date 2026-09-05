package realtime

// Audit HTTP-6 — the documented wiring calls Hub.Shutdown from Service.Stop,
// which the server runs only after http.Server.Shutdown has waited for every
// in-flight request. A live stream is one of those requests, so the drain waited
// on exactly the connections Hub.Shutdown was going to close, and spent the
// whole ShutdownTimeout doing it.
//
// HubConfig.ShuttingDown breaks that: the server announces shutdown before it
// starts waiting, the hub signals its clients then, and the handlers return in
// time for the drain to finish.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// drainTestWriter is an http.ResponseWriter+Flusher that discards everything, so
// the SSE handler runs its normal course without a test hook in the middle of it.
type drainTestWriter struct {
	mu     sync.Mutex
	header http.Header
}

func (w *drainTestWriter) Header() http.Header { return w.header }
func (w *drainTestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(p), nil
}
func (w *drainTestWriter) WriteHeader(int) {}
func (w *drainTestWriter) Flush()          {}

func TestHubShuttingDown_ClosesConnectionsBeforeShutdownIsCalled(t *testing.T) {
	draining := make(chan struct{})
	bus := &synchronousTestBus{}
	hub, err := NewHub(HubConfig{Bus: bus, ShuttingDown: draining})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		hub.Shutdown(ctx) //nolint:errcheck // best effort teardown
	})

	// A live SSE handler, still running: this is the in-flight request the HTTP
	// drain would be waiting on.
	w := &drainTestWriter{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(context.Background())
	handlerDone := make(chan struct{})
	go func() {
		hub.SSEHandler().ServeHTTP(w, req)
		close(handlerDone)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for hub.Stats().Connections == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Stats().Connections == 0 {
		t.Fatal("the SSE client never registered")
	}

	// The server announces shutdown. Nobody has called Hub.Shutdown.
	close(draining)

	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the SSE handler did not return when the server announced shutdown; the " +
			"HTTP drain would wait on it for the whole ShutdownTimeout, and the " +
			"Hub.Shutdown that would have closed it does not run until after that")
	}
}

// Signalling must not mark the hub closed: Service.Stop still calls Shutdown,
// and that call is what waits for the connections to be gone.
func TestHubShuttingDown_LeavesShutdownItsWork(t *testing.T) {
	draining := make(chan struct{})
	bus := &synchronousTestBus{}
	hub, err := NewHub(HubConfig{Bus: bus, ShuttingDown: draining})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	close(draining)
	time.Sleep(50 * time.Millisecond)

	if hub.closed.Load() {
		t.Error("the hub marked itself closed on the signal; Shutdown would then return " +
			"immediately and never drain")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown after the signal: %v", err)
	}
}

// A hub shut down before any signal arrives must not leave its watcher parked on
// a channel that may never close.
func TestHubShuttingDown_WatcherEndsWithTheHub(t *testing.T) {
	draining := make(chan struct{})
	bus := &synchronousTestBus{}
	hub, err := NewHub(HubConfig{Bus: bus, ShuttingDown: draining})
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := hub.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-hub.drainDone:
	default:
		t.Error("the ShuttingDown watcher was not released by Shutdown")
	}
}
