package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
)

// WebhookHandler processes a decoded webhook payload after signature
// verification succeeds. The raw body is supplied so handlers can decode it
// into a type appropriate for the event.
type WebhookHandler func(w http.ResponseWriter, r *http.Request, body []byte) error

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
}

// Handler returns an http.HandlerFunc that, on each request:
//
//  1. Reads at most MaxBodyBytes from the body.
//  2. Computes HMAC over the raw body using Secret + Algorithm.
//  3. Compares the result to the value in HeaderKey (constant-time).
//  4. Looks up handlers[event] using EventHeaderKey.
//  5. Invokes the handler with the raw body.
//
// Failures map to: 400 (decode), 401 (signature mismatch), 404 (no handler
// for the event), or whatever the handler chooses to return.
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

	return func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBytes))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		req.Body.Close()

		got := req.Header.Get(headerKey)
		if got == "" {
			http.Error(w, "missing signature", http.StatusUnauthorized)
			return
		}

		want := computeHMAC(hashFn, []byte(r.Secret), body)
		// Strip common "<algo>=" prefix that GitHub/Stripe and others use.
		if eq := indexByte(got, '='); eq > 0 && eq < len(got)-1 {
			got = got[eq+1:]
		}
		if !hmac.Equal([]byte(got), []byte(want)) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		event := req.Header.Get(eventHeader)
		h, ok := handlers[event]
		if !ok {
			http.Error(w, fmt.Sprintf("no handler for %q", event), http.StatusNotFound)
			return
		}
		if err := h(w, req, body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
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
