package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
