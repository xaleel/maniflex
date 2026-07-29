package maniflex

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestObserverSeesEarlyRouterRejection(t *testing.T) {
	observed := make(chan RequestObservation, 1)
	server := New(Config{
		QueryLimits: QueryLimits{MaxURLBytes: 32},
		RequestObservers: []RequestObserver{
			func(observation RequestObservation) { observed <- observation },
		},
	})

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	response, err := http.Get(httpServer.URL + "/api/" + strings.Repeat("x", 64))
	if err != nil {
		t.Fatalf("GET oversized URI: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusRequestURITooLong {
		t.Fatalf("status = %d, want 414", response.StatusCode)
	}

	observation := <-observed
	if observation.Status != http.StatusRequestURITooLong {
		t.Errorf("observed status = %d, want 414", observation.Status)
	}
	if observation.Model != "" || observation.Operation != "" {
		t.Errorf("early rejection route labels = %q/%q, want empty", observation.Model, observation.Operation)
	}
}

func TestRequestObserverPanicDoesNotCorruptResponse(t *testing.T) {
	server := New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RequestObservers: []RequestObserver{
			func(RequestObservation) { panic("collector unavailable") },
		},
	})

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	response, err := http.Get(httpServer.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET health: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
}

func TestObserveRequestsRejectsLateAndNilRegistration(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		server := New(Config{})
		assertObserverPanic(t, "must not be nil", func() {
			server.ObserveRequests(nil)
		})
	})

	t.Run("late", func(t *testing.T) {
		server := New(Config{})
		_ = server.Handler()
		assertObserverPanic(t, "configuration is sealed", func() {
			server.ObserveRequests(func(RequestObservation) {})
		})
	})
}

func assertObserverPanic(t *testing.T, contains string, fn func()) {
	t.Helper()
	defer func() {
		panicValue := recover()
		if panicValue == nil {
			t.Fatalf("expected panic containing %q", contains)
		}
		if message := panicValue.(string); !strings.Contains(message, contains) {
			t.Fatalf("panic = %q, want substring %q", message, contains)
		}
	}()
	fn()
}
