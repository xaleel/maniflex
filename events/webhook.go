package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// WebhookConfig controls HTTP event delivery.
type WebhookConfig struct {
	// URL to POST each event to.
	URL string
	// Secret signs the JSON body with HMAC-SHA256.
	// The signature is sent in X-Webhook-Signature: sha256=<hex>.
	// Leave empty to disable signing.
	Secret string
	// Timeout for each HTTP POST. Default: 5s.
	Timeout time.Duration
	// MaxRetries is the number of retry attempts on 5xx or network error. Default: 0.
	MaxRetries int
	// Headers are added to every webhook request.
	Headers map[string]string
}

// Webhook returns a Handler that POSTs each received event as JSON to cfg.URL.
// Use it as a subscriber on a Bus:
//
//	bus.Subscribe(ctx, events.Subscription{
//	    Patterns: []string{"invoice.*", "order.*"},
//	    Handler:  events.Webhook(events.WebhookConfig{
//	        URL:    "https://hooks.example.com/receive",
//	        Secret: "whsec_abc123",
//	    }),
//	})
func Webhook(cfg WebhookConfig) Handler {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	client := &http.Client{Timeout: cfg.Timeout}

	return func(ctx context.Context, e Event) error {
		body, err := json.Marshal(e)
		if err != nil {
			return err
		}

		var lastErr error
		for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if e.TraceID != "" {
				req.Header.Set("traceparent", e.TraceID)
			}
			for k, v := range cfg.Headers {
				req.Header.Set(k, v)
			}
			if cfg.Secret != "" {
				mac := hmac.New(sha256.New, []byte(cfg.Secret))
				mac.Write(body)
				req.Header.Set("X-Webhook-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
			}

			resp, err := client.Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil // success or permanent client error
			}
			lastErr = fmt.Errorf("webhook: server responded %d", resp.StatusCode)
		}
		return lastErr
	}
}
