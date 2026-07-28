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
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultCallerTimeout bounds each outbound attempt.
	DefaultCallerTimeout = 10 * time.Second
	// DefaultCallerMaxRetry is the number of attempts after the first.
	DefaultCallerMaxRetry = 3
	// DefaultCallerMaxResponseBytes bounds response bodies, including errors.
	DefaultCallerMaxResponseBytes int64 = 4 << 20

	defaultCallerBackoffCap    = 2 * time.Second
	defaultCallerRetryAfterCap = 30 * time.Second
)

var (
	// ErrResponseTooLarge means an upstream response exceeded MaxResponseBytes.
	ErrResponseTooLarge = errors.New("integration: response body too large")
	// ErrCrossOriginRedirect means the default redirect policy refused to send
	// the request, including its static custom headers, to another origin.
	ErrCrossOriginRedirect = errors.New("integration: cross-origin redirect refused")
)

// NewCaller returns a Caller with production-safe timeout, retry, and response
// size defaults. A struct literal receives the same defaults at call time when
// those fields are zero.
func NewCaller(baseURL string) *Caller {
	return &Caller{
		BaseURL:          baseURL,
		Timeout:          DefaultCallerTimeout,
		MaxRetry:         DefaultCallerMaxRetry,
		MaxResponseBytes: DefaultCallerMaxResponseBytes,
	}
}

// Caller is a JSON-over-HTTP client with built-in retry/backoff. Configure it
// once at startup; reuse the same Caller for every outbound call to a given
// service.
//
// The zero value is unusable — BaseURL is required. All other fields have
// sensible defaults: 10s timeout, 3 retries on 5xx/network errors, a 4 MiB
// response limit, same-origin redirects, and jittered exponential backoff.
// Pass `Headers` to attach static headers (Authorization, X-Api-Key) to every
// request.
type Caller struct {
	// BaseURL is prepended to every request path. Must not be empty.
	BaseURL string

	// Timeout is the per-attempt timeout. Zero means 10 seconds; a negative
	// value explicitly disables the timeout.
	Timeout time.Duration

	// MaxRetry is the number of additional attempts after a failure. Zero means
	// 3; a negative value explicitly disables retries. Network errors, 5xx,
	// and 429 are retriable; other 4xx responses are final.
	MaxRetry int

	// BackoffFn maps the attempt number (1-based) to the sleep duration
	// before the next try. The default is jittered exponential backoff capped
	// at 2 seconds. A valid Retry-After response header takes precedence and
	// is capped at 30 seconds.
	BackoffFn func(attempt int) time.Duration

	// MaxResponseBytes bounds every response body, including non-2xx bodies and
	// responses whose decoded value is discarded. Zero means 4 MiB; a negative
	// value explicitly disables the limit.
	MaxResponseBytes int64

	// Headers are added to every request before per-call headers.
	Headers map[string]string

	// HTTPClient lets callers inject a custom *http.Client (for proxying,
	// tracing, mocking). Caller clones it before use. A nil CheckRedirect gets
	// the safe same-origin policy; set an explicit CheckRedirect to override it.
	// A zero client timeout receives Caller's resolved timeout.
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

	client := c.httpClient()

	backoff := c.BackoffFn
	if backoff == nil {
		backoff = defaultBackoff
	}

	maxResponseBytes := c.MaxResponseBytes
	if maxResponseBytes == 0 {
		maxResponseBytes = DefaultCallerMaxResponseBytes
	}
	maxRetry := c.MaxRetry
	if maxRetry == 0 {
		maxRetry = DefaultCallerMaxRetry
	} else if maxRetry < 0 {
		maxRetry = 0
	}

	var lastErr error
	maxAttempts := maxRetry + 1
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
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			lastErr = rerr
			if errors.Is(rerr, ErrCrossOriginRedirect) {
				return rerr
			}
			if attempt < maxAttempts && !isContextErr(rerr) {
				if !sleepCtx(ctx, backoff(attempt)) {
					return ctx.Err()
				}
				continue
			}
			return rerr
		}

		respBody, readErr := readResponseBody(resp.Body, resp.ContentLength, maxResponseBytes)
		resp.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, ErrResponseTooLarge) {
				return readErr
			}
			lastErr = fmt.Errorf("integration: read response body: %w", readErr)
			if attempt < maxAttempts && !isContextErr(readErr) {
				if !sleepCtx(ctx, backoff(attempt)) {
					return ctx.Err()
				}
				continue
			}
			return lastErr
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out == nil || len(respBody) == 0 {
				return nil
			}
			return json.Unmarshal(respBody, out)
		}

		statusErr := buildStatusErr(resp, respBody)
		lastErr = statusErr
		if statusErr.IsRetriable() && attempt < maxAttempts {
			delay := backoff(attempt)
			if retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				delay = min(retryAfter, defaultCallerRetryAfterCap)
			}
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		return statusErr
	}
	return lastErr
}

func (c *Caller) httpClient() *http.Client {
	client := &http.Client{}
	if c.HTTPClient != nil {
		*client = *c.HTTPClient
	}

	switch {
	case c.Timeout < 0:
		client.Timeout = 0
	case c.Timeout > 0:
		client.Timeout = c.Timeout
	case client.Timeout == 0:
		client.Timeout = DefaultCallerTimeout
	}
	if client.CheckRedirect == nil {
		client.CheckRedirect = sameOriginRedirect
	}
	return client
}

func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 || sameOrigin(via[0].URL, req.URL) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrCrossOriginRedirect, via[0].URL, req.URL)
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func readResponseBody(body io.Reader, contentLength, maxBytes int64) ([]byte, error) {
	if maxBytes < 0 {
		return io.ReadAll(body)
	}
	if contentLength > maxBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResponseTooLarge, maxBytes)
	}
	readLimit := maxBytes + 1
	if maxBytes == math.MaxInt64 {
		readLimit = math.MaxInt64
	}
	data, err := io.ReadAll(io.LimitReader(body, readLimit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrResponseTooLarge, maxBytes)
	}
	return data, nil
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(math.MaxInt64/time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if delay := when.Sub(now); delay > 0 {
		return delay, true
	}
	return 0, true
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
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 20)
	d := (100 * time.Millisecond) << shift
	if d > defaultCallerBackoffCap {
		d = defaultCallerBackoffCap
	}
	// Equal jitter keeps half the delay deterministic and randomises the other
	// half so a replica fleet does not retry in lockstep.
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(d-half)+1))
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
