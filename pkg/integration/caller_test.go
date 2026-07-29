package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// jsonEcho returns a test server that responds with a fixed JSON body and
// records the request count.
func jsonEcho(t *testing.T, status int, body string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestCaller_GetDecodesJSON(t *testing.T) {
	srv, _ := jsonEcho(t, 200, `{"hello": "world", "n": 42}`)
	c := &Caller{BaseURL: srv.URL}
	var out struct {
		Hello string `json:"hello"`
		N     int    `json:"n"`
	}
	if err := c.Get(context.Background(), "/x", nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.Hello != "world" || out.N != 42 {
		t.Errorf("decoded fields wrong: %+v", out)
	}
}

func TestCaller_PostSendsJSON(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL}
	in := map[string]any{"name": "alice", "age": 30}
	if err := c.Post(context.Background(), "/users", in, nil); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !strings.Contains(gotBody, `"name":"alice"`) {
		t.Errorf("body missing name: %q", gotBody)
	}
}

func TestCaller_GetAppliesQueryParams(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL}
	params := url.Values{"q": []string{"hello"}, "limit": []string{"10"}}
	if err := c.Get(context.Background(), "/search", params, nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(gotPath, "q=hello") || !strings.Contains(gotPath, "limit=10") {
		t.Errorf("query string missing params: %q", gotPath)
	}
}

func TestCaller_StaticHeadersApplied(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL, Headers: map[string]string{"Authorization": "Bearer xyz"}}
	if err := c.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer xyz")
	}
}

func TestCaller_RetriesOn5xx(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	t.Cleanup(srv.Close)

	c := &Caller{
		BaseURL:   srv.URL,
		MaxRetry:  3,
		BackoffFn: func(attempt int) time.Duration { return time.Millisecond },
	}
	if err := c.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("expected 3 hits (2 retries + success), got %d", hits)
	}
}

func TestCaller_ZeroValueFieldsApplySafeDefaults(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL}
	if err := c.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("Get with zero-value defaults: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want a default retry", got)
	}

	var deadline time.Time
	c = &Caller{
		BaseURL: "https://example.invalid",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			deadline, _ = req.Context().Deadline()
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})},
	}
	if err := c.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("Get through recording transport: %v", err)
	}
	remaining := time.Until(deadline)
	if deadline.IsZero() || remaining < 9*time.Second || remaining > DefaultCallerTimeout {
		t.Errorf("default deadline remaining = %v, want approximately %v", remaining, DefaultCallerTimeout)
	}
}

func TestNewCallerMatchesDocumentedDefaults(t *testing.T) {
	t.Parallel()

	const baseURL = "https://api.example.test"
	constructed := NewCaller(baseURL)
	if constructed == nil {
		t.Fatal("NewCaller returned nil")
	}
	if constructed.BaseURL != baseURL {
		t.Errorf("BaseURL = %q, want %q", constructed.BaseURL, baseURL)
	}
	if constructed.Timeout != DefaultCallerTimeout {
		t.Errorf("Timeout = %v, want %v", constructed.Timeout, DefaultCallerTimeout)
	}
	if constructed.MaxRetry != DefaultCallerMaxRetry {
		t.Errorf("MaxRetry = %d, want %d", constructed.MaxRetry, DefaultCallerMaxRetry)
	}
	if constructed.MaxResponseBytes != DefaultCallerMaxResponseBytes {
		t.Errorf("MaxResponseBytes = %d, want %d",
			constructed.MaxResponseBytes, DefaultCallerMaxResponseBytes)
	}

	direct := &Caller{BaseURL: baseURL}
	if got := direct.httpClient().Timeout; got != constructed.httpClient().Timeout {
		t.Errorf("direct literal timeout = %v, constructor timeout = %v",
			got, constructed.httpClient().Timeout)
	}
}

func TestCaller_NegativeMaxRetryDisablesRetries(t *testing.T) {
	srv, calls := jsonEcho(t, http.StatusServiceUnavailable, ``)
	c := &Caller{BaseURL: srv.URL, MaxRetry: -1}
	if err := c.Get(context.Background(), "/", nil, nil); err == nil {
		t.Fatal("expected status error")
	}
	if *calls != 1 {
		t.Errorf("calls = %d, want one shot when MaxRetry is negative", *calls)
	}
}

func TestCaller_ResponseBodyLimitRejectsChunkedOverflow(t *testing.T) {
	const limit = int64(32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // force unknown/chunked Content-Length
		_, _ = io.WriteString(w, strings.Repeat("x", int(limit)+1))
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL, MaxResponseBytes: limit, MaxRetry: -1}
	var out any
	err := c.Get(context.Background(), "/", nil, &out)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}
	if out != nil {
		t.Errorf("oversized response partially decoded: %#v", out)
	}
}

func TestCaller_ZeroResponseLimitBoundsDiscardedErrorBody(t *testing.T) {
	c := &Caller{
		BaseURL:  "https://example.invalid",
		MaxRetry: -1,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusBadGateway,
				Status:        "502 Bad Gateway",
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("")),
				ContentLength: DefaultCallerMaxResponseBytes + 1,
				Request:       req,
			}, nil
		})},
	}

	err := c.Get(context.Background(), "/", nil, nil)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}
}

func TestCaller_RejectsCrossOriginRedirectWithoutLeakingCustomHeader(t *testing.T) {
	var targetHits int32
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		leaked = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/target", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	c := &Caller{
		BaseURL:  origin.URL,
		MaxRetry: -1,
		Headers:  map[string]string{"X-API-Key": "private-key"},
	}
	err := c.Get(context.Background(), "/", nil, nil)
	if !errors.Is(err, ErrCrossOriginRedirect) {
		t.Fatalf("error = %v, want ErrCrossOriginRedirect", err)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 || leaked != "" {
		t.Errorf("redirect target was reached: hits=%d leaked header=%q", got, leaked)
	}
}

func TestCaller_AllowsSameOriginRedirect(t *testing.T) {
	var gotHeader string
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/done", http.StatusFound)
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL, Headers: map[string]string{"X-API-Key": "same-origin"}}
	if err := c.Get(context.Background(), "/start", nil, nil); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if gotHeader != "same-origin" {
		t.Errorf("same-origin header = %q, want preserved", gotHeader)
	}
}

func TestCaller_RetryAfterDelaysRetryAndHonorsContext(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{
		BaseURL:   srv.URL,
		MaxRetry:  1,
		BackoffFn: func(int) time.Duration { return 0 },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Get(ctx, "/", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline during Retry-After", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want Retry-After to delay the second attempt", got)
	}
}

func TestDefaultBackoffIsCappedExponentialJitter(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := defaultBackoff(1); got < 50*time.Millisecond || got > 100*time.Millisecond {
			t.Fatalf("attempt 1 backoff = %v, want [50ms,100ms]", got)
		}
		if got := defaultBackoff(2); got < 100*time.Millisecond || got > 200*time.Millisecond {
			t.Fatalf("attempt 2 backoff = %v, want [100ms,200ms]", got)
		}
		if got := defaultBackoff(100); got < time.Second || got > defaultCallerBackoffCap {
			t.Fatalf("capped backoff = %v, want [1s,%v]", got, defaultCallerBackoffCap)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if got, ok := parseRetryAfter("7", now); !ok || got != 7*time.Second {
		t.Errorf("seconds Retry-After = %v, %v", got, ok)
	}
	date := now.Add(3 * time.Second).Format(http.TimeFormat)
	if got, ok := parseRetryAfter(date, now); !ok || got != 3*time.Second {
		t.Errorf("date Retry-After = %v, %v", got, ok)
	}
	if _, ok := parseRetryAfter("invalid", now); ok {
		t.Error("invalid Retry-After accepted")
	}
}

func TestCaller_RetriesOn429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	c := &Caller{BaseURL: srv.URL, MaxRetry: 2,
		BackoffFn: func(int) time.Duration { return time.Millisecond }}
	if err := c.Get(context.Background(), "/", nil, nil); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Errorf("expected retry after 429, got %d hits", hits)
	}
}

func TestCaller_DoesNotRetryOn4xx(t *testing.T) {
	srv, calls := jsonEcho(t, 400, `{"error": "bad"}`)
	c := &Caller{
		BaseURL:   srv.URL,
		MaxRetry:  5,
		BackoffFn: func(int) time.Duration { return time.Millisecond },
	}
	err := c.Get(context.Background(), "/", nil, nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	var statusErr *ErrHTTPStatus
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected *ErrHTTPStatus, got %T", err)
	}
	if statusErr.StatusCode != 400 {
		t.Errorf("StatusCode = %d, want 400", statusErr.StatusCode)
	}
	if *calls != 1 {
		t.Errorf("400 should not retry; got %d calls", *calls)
	}
}

func TestCaller_GivesUpAfterMaxRetry(t *testing.T) {
	srv, calls := jsonEcho(t, 500, ``)
	c := &Caller{
		BaseURL:   srv.URL,
		MaxRetry:  2,
		BackoffFn: func(int) time.Duration { return time.Millisecond },
	}
	err := c.Get(context.Background(), "/", nil, nil)
	if err == nil {
		t.Fatal("expected error after MaxRetry exceeded")
	}
	if *calls != 3 {
		t.Errorf("expected 3 attempts (1 + 2 retries), got %d", *calls)
	}
}

func TestCaller_NonJSONErrorBodyPreserved(t *testing.T) {
	srv, _ := jsonEcho(t, 403, `Forbidden`)
	c := &Caller{BaseURL: srv.URL}
	err := c.Get(context.Background(), "/", nil, nil)
	var se *ErrHTTPStatus
	if !errors.As(err, &se) {
		t.Fatalf("expected *ErrHTTPStatus, got %v", err)
	}
	if se.RawBody != "Forbidden" {
		t.Errorf("RawBody: got %q, want %q", se.RawBody, "Forbidden")
	}
}

func TestCaller_JSONErrorBodyDecoded(t *testing.T) {
	srv, _ := jsonEcho(t, 422, `{"error": "validation failed", "field": "email"}`)
	c := &Caller{BaseURL: srv.URL}
	err := c.Get(context.Background(), "/", nil, nil)
	var se *ErrHTTPStatus
	if !errors.As(err, &se) {
		t.Fatalf("expected *ErrHTTPStatus, got %v", err)
	}
	if se.Body["field"] != "email" {
		t.Errorf("Body.field: got %v, want %q", se.Body["field"], "email")
	}
}

func TestCaller_ContextCancellationAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block long enough for ctx to cancel mid-flight.
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := c.Get(ctx, "/", nil, nil)
	if err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestCaller_EmptyBaseURLErrors(t *testing.T) {
	c := &Caller{}
	if err := c.Get(context.Background(), "/", nil, nil); err == nil {
		t.Error("empty BaseURL should error")
	}
}

func TestCaller_BodyAsBytesPassedThrough(t *testing.T) {
	// Passing raw bytes as body must not re-JSON-encode.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)

	c := &Caller{BaseURL: srv.URL}
	if err := c.Post(context.Background(), "/", []byte("raw payload"), nil); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotBody != "raw payload" {
		t.Errorf("body: got %q, want raw payload", gotBody)
	}
}
