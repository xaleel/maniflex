package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WebhookHandler processes a decoded webhook payload after signature
// verification succeeds. The raw body is supplied so handlers can decode it
// into a type appropriate for the event.
type WebhookHandler func(w http.ResponseWriter, r *http.Request, body []byte) error

// ErrWebhookReplay is returned by WebhookReceiver.ReplayCheck when a signed
// request has already been accepted. The receiver maps it to HTTP 409 without
// invoking the event handler.
var ErrWebhookReplay = errors.New("integration: webhook replay")

// WebhookReplayCheck atomically checks and records a verified webhook request.
//
// Implementations normally extract the provider's event ID from the signed
// body and claim it in a shared store with a TTL. The check runs only after the
// HMAC and optional timestamp have been verified. Return ErrWebhookReplay when
// the event was already claimed; other errors are logged and returned to the
// client as a generic 500.
//
// The check must be atomic to stop two concurrent deliveries of the same event
// from both reaching the handler.
type WebhookReplayCheck func(ctx context.Context, r *http.Request, body []byte) error

// WebhookReceiver verifies an HMAC signature on inbound webhook requests and
// dispatches to a per-event handler. Use it for payment-gateway / e-invoicing
// callbacks where the upstream signs each request with a shared secret.
type WebhookReceiver struct {
	// Secret is the shared HMAC key. Required.
	Secret string

	// Algorithm selects the HMAC hash: "sha256" (default) or "sha512".
	Algorithm string

	// HeaderKey is the request header carrying the hex-encoded signature.
	// Defaults to "X-Hub-Signature-256" (GitHub-style); set to whatever the
	// upstream uses.
	HeaderKey string

	// EventHeaderKey is the request header naming the event type (the dispatch
	// key into the handlers map). Defaults to "X-Event-Type".
	EventHeaderKey string

	// MaxBodyBytes caps the request body size. 0 means 1 MiB.
	MaxBodyBytes int64

	// TimestampHeaderKey enables timestamp-window validation. The named header
	// must contain Unix seconds or an RFC3339 timestamp, and the signature must
	// cover "<raw timestamp>.<raw body>" instead of only the body. Empty
	// disables timestamp validation.
	TimestampHeaderKey string

	// TimestampTolerance is the maximum accepted clock skew in either
	// direction when TimestampHeaderKey is set. Zero means 5 minutes.
	TimestampTolerance time.Duration

	// Clock supplies the current time for timestamp validation. Nil uses
	// time.Now. It is primarily useful for deterministic tests.
	Clock func() time.Time

	// ReplayCheck optionally performs atomic event-ID deduplication after the
	// signature and timestamp have been verified. Return ErrWebhookReplay for
	// a duplicate delivery.
	ReplayCheck WebhookReplayCheck

	// Logger receives handler failures with their private diagnostic text.
	// Defaults to slog.Default. Clients receive only a generic 500 response.
	Logger *slog.Logger
}

// Handler returns an http.HandlerFunc that, on each request:
//
//  1. Reads at most MaxBodyBytes from the body, rejecting overflow with 413.
//  2. Computes HMAC over the raw body using Secret + Algorithm.
//  3. Compares the result to the value in HeaderKey (constant-time).
//  4. Applies optional timestamp-window and replay checks.
//  5. Looks up handlers[event] using EventHeaderKey.
//  6. Invokes the handler with the raw body.
//
// Failures map to: 400 (read), 401 (signature/timestamp), 409 (replay), 413
// (oversized body), 404 (no handler), or a generic 500 for a handler/replay
// backend error. Private diagnostics are written only to the configured Logger.
func (r *WebhookReceiver) Handler(handlers map[string]WebhookHandler) http.HandlerFunc {
	if r.Secret == "" {
		panic("integration: WebhookReceiver.Secret must not be empty")
	}
	algo := r.Algorithm
	if algo == "" {
		algo = "sha256"
	}
	hashFn, err := newHashFn(algo)
	if err != nil {
		panic(err)
	}
	headerKey := r.HeaderKey
	if headerKey == "" {
		headerKey = "X-Hub-Signature-256"
	}
	eventHeader := r.EventHeaderKey
	if eventHeader == "" {
		eventHeader = "X-Event-Type"
	}
	maxBytes := r.MaxBodyBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20 // 1 MiB
	}
	if maxBytes < 0 {
		panic("integration: WebhookReceiver.MaxBodyBytes must not be negative")
	}
	timestampHeader := strings.TrimSpace(r.TimestampHeaderKey)
	timestampTolerance := r.TimestampTolerance
	if timestampHeader != "" && timestampTolerance == 0 {
		timestampTolerance = 5 * time.Minute
	}
	if timestampTolerance < 0 {
		panic("integration: WebhookReceiver.TimestampTolerance must not be negative")
	}
	now := r.Clock
	if now == nil {
		now = time.Now
	}

	return func(w http.ResponseWriter, req *http.Request) {
		defer req.Body.Close()
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBytes))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		got := req.Header.Get(headerKey)
		if got == "" {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}

		signedBody := body
		rawTimestamp := ""
		if timestampHeader != "" {
			rawTimestamp = req.Header.Get(timestampHeader)
			if rawTimestamp == "" {
				http.Error(w, "missing webhook timestamp", http.StatusUnauthorized)
				return
			}
			signedBody = make([]byte, 0, len(rawTimestamp)+1+len(body))
			signedBody = append(signedBody, rawTimestamp...)
			signedBody = append(signedBody, '.')
			signedBody = append(signedBody, body...)
		}

		want := computeHMAC(hashFn, []byte(r.Secret), signedBody)
		// Strip common "<algo>=" prefix that GitHub/Stripe and others use.
		if eq := indexByte(got, '='); eq > 0 && eq < len(got)-1 {
			got = got[eq+1:]
		}
		if !hmac.Equal([]byte(got), []byte(want)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		if timestampHeader != "" {
			signedAt, parseErr := parseWebhookTimestamp(rawTimestamp)
			skew := now().Sub(signedAt)
			if parseErr != nil || skew > timestampTolerance || skew < -timestampTolerance {
				http.Error(w, "invalid webhook timestamp", http.StatusUnauthorized)
				return
			}
		}

		event := req.Header.Get(eventHeader)
		h, ok := handlers[event]
		if !ok {
			http.Error(w, fmt.Sprintf("no handler for %q", event), http.StatusNotFound)
			return
		}

		if r.ReplayCheck != nil {
			if replayErr := r.ReplayCheck(req.Context(), req, body); replayErr != nil {
				if errors.Is(replayErr, ErrWebhookReplay) {
					http.Error(w, "webhook replay rejected", http.StatusConflict)
					return
				}
				r.logFailure(req, "webhook replay check failed", replayErr)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}

		if err := h(w, req, body); err != nil {
			r.logFailure(req, "webhook handler failed", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}
}

func (r *WebhookReceiver) logFailure(req *http.Request, message string, err error) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{slog.String("error", err.Error())}
	if requestID := req.Header.Get("X-Request-Id"); requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	logger.Error(message, attrs...)
}

func parseWebhookTimestamp(raw string) (time.Time, error) {
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(seconds, 0), nil
	}
	return time.Parse(time.RFC3339, raw)
}

func newHashFn(algo string) (func() hash.Hash, error) {
	switch algo {
	case "sha256":
		return sha256.New, nil
	case "sha512":
		return sha512.New, nil
	}
	return nil, errors.New("integration: WebhookReceiver.Algorithm must be \"sha256\" or \"sha512\"")
}

func computeHMAC(fn func() hash.Hash, secret, body []byte) string {
	mac := hmac.New(fn, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// indexByte is a tiny local helper to avoid a strings import for one call.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
