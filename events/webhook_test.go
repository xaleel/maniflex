package events_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maniflex/events"
)

func webhookEvent() events.Event {
	return events.Event{
		ID:      "wh-1",
		Type:    "invoice.created",
		Source:  "billing",
		Time:    time.Now().UTC(),
		Data:    json.RawMessage(`{"amount":50}`),
		TraceID: "00-trace000000000000000000-span0000-01",
	}
}

// TestWebhook_SuccessfulDelivery verifies that a 2xx response produces nil error.
func TestWebhook_SuccessfulDelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := events.Webhook(events.WebhookConfig{URL: srv.URL, Timeout: time.Second})
	if err := h(context.Background(), webhookEvent()); err != nil {
		t.Fatalf("expected nil error on 2xx, got: %v", err)
	}
}

// TestWebhook_ClientErrorIsNotRetried verifies that a 4xx response is treated
// as a permanent failure (not retried) and returns nil (non-retriable).
func TestWebhook_ClientErrorIsNotRetried(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	h := events.Webhook(events.WebhookConfig{URL: srv.URL, MaxRetries: 3, Timeout: time.Second})
	h(context.Background(), webhookEvent()) //nolint:errcheck

	if callCount != 1 {
		t.Fatalf("4xx response should not be retried; got %d calls, want 1", callCount)
	}
}

// TestWebhook_POSTsCorrectBody verifies JSON body and Content-Type header.
func TestWebhook_POSTsCorrectBody(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: got %q, want application/json", ct)
		}
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := webhookEvent()
	h := events.Webhook(events.WebhookConfig{URL: srv.URL, Timeout: time.Second})
	h(context.Background(), e) //nolint:errcheck

	var got events.Event
	if err := json.Unmarshal(received, &got); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	if got.ID != e.ID {
		t.Errorf("body ID: got %q, want %q", got.ID, e.ID)
	}
}

// TestWebhook_HMACSignature verifies the X-Webhook-Signature header is set
// when a secret is configured.
func TestWebhook_HMACSignature(t *testing.T) {
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-Webhook-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := events.Webhook(events.WebhookConfig{URL: srv.URL, Secret: "supersecret", Timeout: time.Second})
	h(context.Background(), webhookEvent()) //nolint:errcheck

	if !strings.HasPrefix(gotSig, "sha256=") {
		t.Fatalf("X-Webhook-Signature: got %q, want sha256=... prefix", gotSig)
	}
}

// TestWebhook_TraceparentHeader verifies the traceparent header is forwarded.
func TestWebhook_TraceparentHeader(t *testing.T) {
	var gotTrace string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTrace = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := webhookEvent()
	h := events.Webhook(events.WebhookConfig{URL: srv.URL, Timeout: time.Second})
	h(context.Background(), e) //nolint:errcheck

	if gotTrace != e.TraceID {
		t.Errorf("traceparent: got %q, want %q", gotTrace, e.TraceID)
	}
}

// TestWebhook_ExhaustedRetries_ReturnsError is a RED test for E12.
//
// After MaxRetries 5xx responses the Webhook handler currently returns nil,
// silently swallowing the failure. This prevents the subscription-level retry
// loop (DeliverWithRetry) from ever seeing an error and the event is lost.
//
// Fix (E12): return a non-nil error when all attempts fail, and remove the
// in-handler time.Sleep — let DeliverWithRetry handle backoff uniformly.
func TestWebhook_ExhaustedRetries_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	h := events.Webhook(events.WebhookConfig{
		URL:        srv.URL,
		MaxRetries: 1,
		Timeout:    200 * time.Millisecond,
	})

	err := h(context.Background(), webhookEvent())
	// E12: currently returns nil after all retries → test FAILS (red).
	// After fix: returns non-nil → test PASSES (green).
	if err == nil {
		t.Fatal("expected non-nil error after all retries exhausted on 503 responses (E12: Webhook swallows failure silently)")
	}
}
