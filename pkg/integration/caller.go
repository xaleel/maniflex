// Package integration provides building blocks for outbound integrations:
// HTTP callers with retry/backoff, periodic pollers, and signed-webhook
// receivers. It's deliberately small — the framework owns the request
// pipeline, not the integration patterns that sit beside it.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Caller is a JSON-over-HTTP client with built-in retry/backoff. Configure it
// once at startup; reuse the same Caller for every outbound call to a given
// service.
//
// The zero value is unusable — BaseURL is required. All other fields have
// sensible defaults: 10s timeout, 3 retries on 5xx + network errors, and a
// linear backoff capped at 1s per retry. Pass `Headers` to attach static
// headers (Authorization, X-Api-Key) to every request.
type Caller struct {
	// BaseURL is prepended to every request path. Must not be empty.
	BaseURL string

	// Timeout is the per-request timeout. 0 means no timeout.
	Timeout time.Duration

	// MaxRetry is the number of additional attempts after a failure. 0 means
	// no retries (one shot). Network errors and 5xx responses count as
	// retriable; 4xx is final.
	MaxRetry int

	// BackoffFn maps the attempt number (1-based) to the sleep duration
	// before the next try. Defaults to attempt * 100ms.
	BackoffFn func(attempt int) time.Duration

	// Headers are added to every request before per-call headers.
	Headers map[string]string

	// HTTPClient lets callers inject a custom *http.Client (for proxying,
	// tracing, mocking). When nil, Caller uses a private client whose
	// Timeout matches Caller.Timeout.
	HTTPClient *http.Client
}

// Get performs a GET request to path with the supplied query params and
// decodes the JSON body into out. Pass out=nil to discard the body.
func (c *Caller) Get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, nil, out)
}

// Post performs a POST request with body JSON-encoded as the request body and
// decodes the JSON response into out. Pass out=nil to discard the response.
func (c *Caller) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// Put performs a PUT request. Same semantics as Post.
func (c *Caller) Put(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPut, path, nil, body, out)
}

// Delete performs a DELETE request. body may be nil. out may be nil.
func (c *Caller) Delete(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, body, out)
}

// ErrHTTPStatus is returned when the upstream responds with a non-2xx
// status. The body — if it parses as JSON — is decoded into Body; otherwise
// it's left as a raw string in RawBody.
type ErrHTTPStatus struct {
	StatusCode int
	Status     string
	Body       map[string]any
	RawBody    string
}

func (e *ErrHTTPStatus) Error() string {
	if e.RawBody == "" {
		return fmt.Sprintf("integration: %s", e.Status)
	}
	return fmt.Sprintf("integration: %s: %s", e.Status, truncate(e.RawBody, 200))
}

// IsRetriable reports whether a status code is worth retrying. 5xx and 429
// are retriable; 4xx (except 429) is final.
func (e *ErrHTTPStatus) IsRetriable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func (c *Caller) do(ctx context.Context, method, path string, params url.Values, body, out any) error {
	if c.BaseURL == "" {
		return errors.New("integration: Caller.BaseURL is empty")
	}
	fullURL := strings.TrimRight(c.BaseURL, "/") + ensureLeadingSlash(path)
	if len(params) > 0 {
		sep := "?"
		if strings.Contains(fullURL, "?") {
			sep = "&"
		}
		fullURL = fullURL + sep + params.Encode()
	}

	bodyBytes, err := encodeBody(body)
	if err != nil {
		return err
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: c.Timeout}
	}

	backoff := c.BackoffFn
	if backoff == nil {
		backoff = defaultBackoff
	}

	var lastErr error
	maxAttempts := c.MaxRetry + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Re-prepare the request each attempt — the Body is consumed by Do.
		req, rerr := http.NewRequestWithContext(ctx, method, fullURL,
			bodyReader(bodyBytes))
		if rerr != nil {
			return rerr
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range c.Headers {
			req.Header.Set(k, v)
		}

		resp, rerr := client.Do(req)
		if rerr != nil {
			lastErr = rerr
			if attempt < maxAttempts && !isContextErr(rerr) {
				if !sleepCtx(ctx, backoff(attempt)) {
					return ctx.Err()
				}
				continue
			}
			return rerr
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(respBody) == 0 {
				return nil
			}
			return json.Unmarshal(respBody, out)
		}

		statusErr := buildStatusErr(resp, respBody)
		lastErr = statusErr
		if statusErr.IsRetriable() && attempt < maxAttempts {
			if !sleepCtx(ctx, backoff(attempt)) {
				return ctx.Err()
			}
			continue
		}
		return statusErr
	}
	return lastErr
}

func buildStatusErr(resp *http.Response, body []byte) *ErrHTTPStatus {
	e := &ErrHTTPStatus{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		RawBody:    string(body),
	}
	// Best-effort JSON decode; ignore failures.
	if len(body) > 0 && body[0] == '{' {
		_ = json.Unmarshal(body, &e.Body)
	}
	return e
}

func encodeBody(body any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	switch v := body.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	}
	return json.Marshal(body)
}

func bodyReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func defaultBackoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 100 * time.Millisecond
	if d > time.Second {
		return time.Second
	}
	return d
}

// sleepCtx blocks for d or until ctx is cancelled. Returns false when ctx
// cancelled (so callers can bubble up the ctx error).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func ensureLeadingSlash(p string) string {
	if p == "" || p[0] == '/' {
		return p
	}
	return "/" + p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
