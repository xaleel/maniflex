package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"hash"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testSecret = "super-secret"

// sign computes the hex HMAC over body and optionally prefixes with "algo=".
func sign(t *testing.T, fn func() hash.Hash, body string, prefix string) string {
	t.Helper()
	m := hmac.New(fn, []byte(testSecret))
	m.Write([]byte(body))
	out := hex.EncodeToString(m.Sum(nil))
	if prefix != "" {
		return prefix + "=" + out
	}
	return out
}

// makeReq builds a POST request with body, event header, and signature.
func makeReq(t *testing.T, body, event, sig string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(body))
	r.Header.Set("X-Event-Type", event)
	r.Header.Set("X-Hub-Signature-256", sig)
	return r
}

func TestWebhook_ValidSignatureRoutesToHandler(t *testing.T) {
	var called bool
	wh := &WebhookReceiver{Secret: testSecret}
	h := wh.Handler(map[string]WebhookHandler{
		"order.created": func(w http.ResponseWriter, r *http.Request, body []byte) error {
			called = true
			if string(body) != `{"id":"o-1"}` {
				t.Errorf("body to handler: got %q", body)
			}
			w.WriteHeader(http.StatusOK)
			return nil
		},
	})

	sig := sign(t, sha256.New, `{"id":"o-1"}`, "")
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, `{"id":"o-1"}`, "order.created", sig))
	if !called {
		t.Fatal("handler not invoked")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
}

func TestWebhook_InvalidSignatureRejected(t *testing.T) {
	wh := &WebhookReceiver{Secret: testSecret}
	h := wh.Handler(map[string]WebhookHandler{
		"x": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			t.Error("handler must not run on bad signature")
			return nil
		},
	})
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, `{}`, "x", "deadbeef"))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestWebhook_MissingSignatureRejected(t *testing.T) {
	wh := &WebhookReceiver{Secret: testSecret}
	h := wh.Handler(map[string]WebhookHandler{
		"x": func(w http.ResponseWriter, _ *http.Request, _ []byte) error { return nil },
	})
	r := httptest.NewRequest("POST", "/hook", strings.NewReader(`{}`))
	r.Header.Set("X-Event-Type", "x")
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

func TestWebhook_UnknownEventIs404(t *testing.T) {
	wh := &WebhookReceiver{Secret: testSecret}
	h := wh.Handler(map[string]WebhookHandler{
		"known": func(w http.ResponseWriter, _ *http.Request, _ []byte) error { return nil },
	})
	body := `{}`
	sig := sign(t, sha256.New, body, "")
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, body, "unknown.event", sig))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestWebhook_PrefixedSignatureAccepted(t *testing.T) {
	// GitHub-style "sha256=hex" prefix.
	wh := &WebhookReceiver{Secret: testSecret}
	called := false
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			called = true
			w.WriteHeader(200)
			return nil
		},
	})
	body := `{"x":1}`
	sig := sign(t, sha256.New, body, "sha256")
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, body, "e", sig))
	if !called {
		t.Errorf("prefixed signature should validate; status=%d body=%s",
			rec.Code, rec.Body.String())
	}
}

func TestWebhook_SHA512Algorithm(t *testing.T) {
	wh := &WebhookReceiver{Secret: testSecret, Algorithm: "sha512"}
	called := false
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			called = true
			w.WriteHeader(200)
			return nil
		},
	})
	body := `{}`
	sig := sign(t, sha512.New, body, "")
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, body, "e", sig))
	if !called {
		t.Errorf("sha512 signature should validate; status=%d", rec.Code)
	}
}

func TestWebhook_MaxBodyBytesRejectsSignedPrefixWith413(t *testing.T) {
	wh := &WebhookReceiver{Secret: testSecret, MaxBodyBytes: 4}
	called := false
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			called = true
			return nil
		},
	})
	// Signing the first four bytes exploited the old LimitReader path: it
	// silently transformed this five-byte request into the signed prefix and
	// dispatched it. Overflow must be detected before signature verification.
	full := "12345"
	sig := sign(t, sha256.New, full[:4], "")
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, full, "e", sig))
	if called {
		t.Error("oversized request must not dispatch its signed prefix")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d, want 413", rec.Code)
	}
}

func TestWebhook_MaxBodyBytesAllowsExactLimit(t *testing.T) {
	const body = "1234"
	wh := &WebhookReceiver{Secret: testSecret, MaxBodyBytes: int64(len(body))}
	called := false
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, got []byte) error {
			called = true
			if string(got) != body {
				t.Errorf("body = %q, want exact unmodified payload", got)
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, body, "e", sign(t, sha256.New, body, "")))
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("exact-limit request: called=%v status=%d", called, rec.Code)
	}
}

func TestWebhook_TimestampWindowIsSignedAndEnforced(t *testing.T) {
	fixedNow := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	wh := &WebhookReceiver{
		Secret:             testSecret,
		TimestampHeaderKey: "X-Webhook-Timestamp",
		TimestampTolerance: 5 * time.Minute,
		Clock:              func() time.Time { return fixedNow },
	}
	calls := 0
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			calls++
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})
	const body = `{"id":"evt-1"}`

	request := func(timestamp string, signedTimestamp string) *httptest.ResponseRecorder {
		t.Helper()
		signed := signedTimestamp + "." + body
		req := makeReq(t, body, "e", sign(t, sha256.New, signed, ""))
		req.Header.Set("X-Webhook-Timestamp", timestamp)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	fresh := strconv.FormatInt(fixedNow.Add(-4*time.Minute).Unix(), 10)
	if rec := request(fresh, fresh); rec.Code != http.StatusNoContent {
		t.Fatalf("fresh timestamp status = %d, want 204: %s", rec.Code, rec.Body)
	}

	stale := strconv.FormatInt(fixedNow.Add(-6*time.Minute).Unix(), 10)
	if rec := request(stale, stale); rec.Code != http.StatusUnauthorized {
		t.Errorf("stale timestamp status = %d, want 401", rec.Code)
	}

	// A replay cannot refresh its timestamp header because the raw timestamp is
	// part of the HMAC input.
	refreshed := strconv.FormatInt(fixedNow.Unix(), 10)
	if rec := request(refreshed, stale); rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered timestamp status = %d, want 401", rec.Code)
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want only the fresh request", calls)
	}
}

func TestWebhook_ReplayCheckRejectsDuplicateBeforeHandler(t *testing.T) {
	seen := false
	checks := 0
	wh := &WebhookReceiver{
		Secret: testSecret,
		ReplayCheck: func(_ context.Context, _ *http.Request, _ []byte) error {
			checks++
			if seen {
				return ErrWebhookReplay
			}
			seen = true
			return nil
		},
	}
	calls := 0
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			calls++
			w.WriteHeader(http.StatusNoContent)
			return nil
		},
	})
	const body = `{"id":"evt-1"}`
	sig := sign(t, sha256.New, body, "")

	first := httptest.NewRecorder()
	h(first, makeReq(t, body, "e", sig))
	second := httptest.NewRecorder()
	h(second, makeReq(t, body, "e", sig))

	if first.Code != http.StatusNoContent {
		t.Errorf("first status = %d, want 204", first.Code)
	}
	if second.Code != http.StatusConflict {
		t.Errorf("replay status = %d, want 409", second.Code)
	}
	if calls != 1 || checks != 2 {
		t.Errorf("handler calls = %d, replay checks = %d; want 1 and 2", calls, checks)
	}
}

func TestWebhook_ReplayCheckFailureIsPrivate(t *testing.T) {
	var logs bytes.Buffer
	const privateError = "redis auth failed: password=replay-secret"
	wh := &WebhookReceiver{
		Secret: testSecret,
		ReplayCheck: func(_ context.Context, _ *http.Request, _ []byte) error {
			return errors.New(privateError)
		},
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
	}
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(http.ResponseWriter, *http.Request, []byte) error {
			t.Error("handler must not run when replay storage fails")
			return nil
		},
	})
	const body = `{}`
	rec := httptest.NewRecorder()
	h(rec, makeReq(t, body, "e", sign(t, sha256.New, body, "")))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got != "internal server error\n" {
		t.Errorf("body = %q, want generic 500", got)
	}
	if strings.Contains(rec.Body.String(), privateError) {
		t.Errorf("replay backend error leaked to client: %q", rec.Body)
	}
	if !strings.Contains(logs.String(), privateError) {
		t.Errorf("private replay backend error was not logged: %s", &logs)
	}
}

func TestWebhook_HandlerErrorBecomes500(t *testing.T) {
	var logs bytes.Buffer
	const privateError = "dial tcp 10.0.0.8:5432: password=webhook-secret"
	wh := &WebhookReceiver{
		Secret: testSecret,
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
	}
	h := wh.Handler(map[string]WebhookHandler{
		"e": func(w http.ResponseWriter, _ *http.Request, _ []byte) error {
			return errors.New(privateError)
		},
	})
	body := `{}`
	sig := sign(t, sha256.New, body, "")
	rec := httptest.NewRecorder()
	req := makeReq(t, body, "e", sig)
	req.Header.Set("X-Request-Id", "webhook-request-42")
	h(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got != "internal server error\n" {
		t.Errorf("body: got %q, want generic 500", got)
	}
	if strings.Contains(rec.Body.String(), privateError) {
		t.Errorf("private handler error leaked to client: %q", rec.Body.String())
	}
	if got := logs.String(); !strings.Contains(got, privateError) ||
		!strings.Contains(got, "webhook-request-42") {
		t.Errorf("private error and request ID must be logged; got:\n%s", got)
	}
}

func TestWebhook_EmptySecretPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic when Secret is empty")
		}
	}()
	(&WebhookReceiver{}).Handler(nil)
}

func TestWebhook_UnknownAlgorithmPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for unknown algorithm")
		}
	}()
	(&WebhookReceiver{Secret: "x", Algorithm: "md5"}).Handler(nil)
}
