package maniflex

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// readResult carries what the handler saw, so the assertions are about the
// server's read of the body rather than about parsing a response off the wire.
type readResult struct {
	n   int
	err error
}

// bodyTimeoutServer serves h behind the body read deadline and returns the
// listener address for a raw client.
func bodyTimeoutServer(t *testing.T, d time.Duration, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(bodyReadDeadline(d)(h))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// readingHandler drains the body and reports the outcome exactly once.
func readingHandler(out chan<- readResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		out <- readResult{n: len(b), err: err}
		w.WriteHeader(http.StatusOK)
	})
}

// postHeaders opens a connection and announces a body without sending it, which
// is the whole trick: the headers are complete, so ReadHeaderTimeout is
// satisfied and out of the picture from here on.
func postHeaders(t *testing.T, addr string, contentLength int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: %d\r\n\r\n", contentLength)
	return conn
}

// A client that announces a body and then says nothing must not be able to hold
// the connection open. This is the hole ReadHeaderTimeout leaves and the size
// cap cannot close, since a byte count says nothing about time.
func TestBodyReadDeadline_SilentBodyIsCutOff(t *testing.T) {
	out := make(chan readResult, 1)
	addr := bodyTimeoutServer(t, 200*time.Millisecond, readingHandler(out))

	postHeaders(t, addr, 10) // ...and then nothing at all

	select {
	case got := <-out:
		if got.err == nil {
			t.Fatalf("read returned no error after the client went silent (%d bytes)", got.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still blocked on the body: the deadline never fired")
	}
}

// The reason this can be on by default when ReadTimeout cannot: a slow upload
// that keeps making progress is never severed. Ten bytes at 80ms apart run to
// 800ms, well past the 300ms deadline, and every one of them pushes it out
// again. A whole-request deadline would have killed this at 300ms.
func TestBodyReadDeadline_SlowButProgressingUploadSurvives(t *testing.T) {
	out := make(chan readResult, 1)
	addr := bodyTimeoutServer(t, 300*time.Millisecond, readingHandler(out))

	conn := postHeaders(t, addr, 10)
	go func() {
		for range 10 {
			time.Sleep(80 * time.Millisecond)
			conn.Write([]byte("x"))
		}
	}()

	select {
	case got := <-out:
		if got.err != nil {
			t.Fatalf("a progressing upload was cut off: %v (read %d bytes)", got.err, got.n)
		}
		if got.n != 10 {
			t.Fatalf("read %d bytes, want all 10", got.n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never completed")
	}
}

// The deadline must not outlive the read phase. A handler that finishes reading
// and then streams for a while would otherwise be killed by its own body
// deadline — recreating the exact failure that keeps WriteTimeout unset.
func TestBodyReadDeadline_DoesNotSeverAStreamingResponse(t *testing.T) {
	done := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		for range 4 { // 4 × 150ms = 600ms, twice the 300ms body deadline
			time.Sleep(150 * time.Millisecond)
			if _, err := w.Write([]byte("tick\n")); err != nil {
				done <- err
				return
			}
			rc.Flush()
		}
		done <- nil
	})
	addr := bodyTimeoutServer(t, 300*time.Millisecond, h)

	conn := postHeaders(t, addr, 2)
	conn.Write([]byte("hi"))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streaming response severed by the body read deadline: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the streaming handler never finished")
	}
}

// bodyTimeoutModel gives the router a create route, which is a path that
// actually reads the request body — a 404 would answer without reading one and
// the test would prove nothing.
type bodyTimeoutModel struct {
	BaseModel
	Title string `json:"title" mfx:"required"`
}

// The middleware is worth nothing unless the router installs it. Assembled the
// way an application assembles one, a create request that announces a body and
// then falls silent must be resolved rather than parked.
func TestServerBoot_InstallsTheBodyReadDeadline(t *testing.T) {
	srv := New(Config{BodyReadTimeout: 200 * time.Millisecond})
	srv.MustRegister(bodyTimeoutModel{})
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST /api/body_timeout_models HTTP/1.1\r\nHost: test\r\n"+
		"Content-Type: application/json\r\nContent-Length: 64\r\n\r\n")

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("server never resolved the silent body: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("server took %v to resolve a silent body; the deadline is not installed", elapsed)
	}
	// A real response, not just a dropped connection. Getting one proves the
	// drain net/http runs after the handler returns was bounded too: an unread
	// body is drained so the connection can be reused, and that drain reads from
	// the same silent client. Left unbounded it parks the connection — the file
	// descriptor this whole deadline exists to release.
	if status := string(buf[:n]); !strings.HasPrefix(status, "HTTP/1.1 400") {
		t.Fatalf("want a 400 for the unreadable body, got %q", status)
	}
}

// An explicit ReadTimeout is a stricter bound the caller chose; the idle
// deadline must not quietly extend past it on every read.
func TestServerBoot_ExplicitReadTimeoutDisplacesTheIdleDeadline(t *testing.T) {
	cfg := Config{ReadTimeout: 30 * time.Second, BodyReadTimeout: 200 * time.Millisecond}
	if got := effectiveBodyReadTimeout(&cfg); got != 0 {
		t.Fatalf("BodyReadTimeout = %v with ReadTimeout set, want 0 (disengaged)", got)
	}
}

// With no ReadTimeout the configured idle bound is what applies.
func TestEffectiveBodyReadTimeout_AppliesWithoutAReadTimeout(t *testing.T) {
	cfg := Config{BodyReadTimeout: 200 * time.Millisecond}
	if got := effectiveBodyReadTimeout(&cfg); got != 200*time.Millisecond {
		t.Fatalf("BodyReadTimeout = %v, want the configured 200ms", got)
	}
}

// Negative disables, matching ReadHeaderTimeout and IdleTimeout.
func TestEffectiveBodyReadTimeout_NegativeDisables(t *testing.T) {
	cfg := Config{BodyReadTimeout: -1}
	if got := effectiveBodyReadTimeout(&cfg); got != 0 {
		t.Fatalf("BodyReadTimeout = %v for a negative setting, want 0 (disabled)", got)
	}
}

// nonReadingHandler answers without touching the body — an auth rejection, a
// probe, a 404. Nothing in the wrapper runs on such a route, so anything the
// bound depends on having been armed by a Read is not armed here.
func nonReadingHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
}

// A route that never reads the body must still be bounded. net/http drains what
// the client announced and never sent so the connection can be reused, and that
// drain reads from the same silent client — a slowloris an unauthenticated
// caller can run against any GET, probe, 401, 404 or 405 (audit HTTP-1).
func TestBodyReadDeadline_NonReadingHandlerIsBounded(t *testing.T) {
	addr := bodyTimeoutServer(t, 200*time.Millisecond, nonReadingHandler(http.StatusUnauthorized))

	conn := postHeaders(t, addr, 10) // ...and then nothing at all

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("a silent body on a non-reading route was never resolved: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v to answer; the drain net/http runs was not bounded", elapsed)
	}
	if status := string(buf[:n]); !strings.HasPrefix(status, "HTTP/1.1 401") {
		t.Fatalf("got %q, want the handler's 401", status)
	}
}

// The same client trick with no Content-Length at all: a chunked body whose
// terminating chunk never arrives.
func TestBodyReadDeadline_NonReadingHandlerBoundsAChunkedBody(t *testing.T) {
	addr := bodyTimeoutServer(t, 200*time.Millisecond, nonReadingHandler(http.StatusOK))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: test\r\nTransfer-Encoding: chunked\r\n\r\n")

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 64)); err != nil {
		t.Fatalf("a silent chunked body was never resolved: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v to answer", elapsed)
	}
}

// Once the response outgrows net/http's 2 KiB buffer the drain runs from inside
// the handler rather than after it, so a bound armed on the way out arrives too
// late. This is the shape of an ordinary list response: several kilobytes, and
// not a byte of the request body read. Under a concurrency limit the stuck
// handler also holds its slot.
func TestBodyReadDeadline_LargeResponseFromANonReadingHandlerIsBounded(t *testing.T) {
	done := make(chan time.Duration, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		start := time.Now()
		w.Write(make([]byte, 8<<10))
		done <- time.Since(start)
	})
	addr := bodyTimeoutServer(t, 200*time.Millisecond, h)

	postHeaders(t, addr, 10)

	select {
	case took := <-done:
		if took > 2*time.Second {
			t.Fatalf("the handler spent %v inside net/http's mid-response drain", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still blocked inside net/http's mid-response drain")
	}
}

// A body read to EOF clears the bound, because net/http starts the background
// read that would trip it at exactly that point. This is the same guarantee
// TestBodyReadDeadline_DoesNotSeverAStreamingResponse asserts end to end, stated
// against the wrapper itself so a change to the clearing rule is caught here.
func TestBodyReadDeadline_ClearsTheDeadlineAtEOF(t *testing.T) {
	var cleared bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		// A deadline still in force would bound the response; setting one far
		// out and reading the body again is not observable from here, so assert
		// on the wrapper's own state instead.
		if b, ok := r.Body.(*deadlineBody); ok {
			cleared = !b.unsupported
		}
		w.WriteHeader(http.StatusOK)
	})
	addr := bodyTimeoutServer(t, 300*time.Millisecond, h)

	conn := postHeaders(t, addr, 2)
	conn.Write([]byte("hi"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 64)); err != nil {
		t.Fatalf("no response: %v", err)
	}
	if !cleared {
		t.Fatal("the connection refused a deadline; this test proves nothing")
	}
}

// The load shedder answers 503 without reading a body, so the same drain applies
// to a shed request and the deadline has to be installed ahead of it.
func TestServerBoot_BodyDeadlinePrecedesLoadShedding(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	hold := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("hold") != "" {
				entered <- struct{}{}
				<-gate
			}
			next.ServeHTTP(w, r)
		})
	}
	srv := New(Config{
		BodyReadTimeout:       200 * time.Millisecond,
		MaxConcurrentRequests: 1,
		HTTPMiddlewares:       []HTTPMiddleware{hold},
	})
	srv.MustRegister(bodyTimeoutModel{})
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	ts := httptest.NewServer(h)
	closeOnce := sync.OnceFunc(func() { close(gate) })
	// Registered first so it runs last: Close waits for the held request.
	t.Cleanup(ts.Close)
	t.Cleanup(closeOnce)

	// Occupy the only slot, and wait until it actually is. Polling for a 503
	// instead would race: the polling request can take the slot meant for the
	// holding one, and then nothing is held at all. A model route rather than a
	// probe, since probes take no slot (see TestConcurrencyLimit_ProbesAreNotShed).
	go http.Get(ts.URL + "/api/body_timeout_models?hold=1") //nolint:errcheck // released at cleanup
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the holding request never reached the middleware")
	}

	// Now the shed request, announcing a body it never sends.
	conn, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST /api/body_timeout_models HTTP/1.1\r\nHost: test\r\n"+
		"Content-Type: application/json\r\nContent-Length: 64\r\n\r\n")

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("a shed request with a silent body was never resolved: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %v; the deadline is installed behind the load shedder", elapsed)
	}
	if status := string(buf[:n]); !strings.HasPrefix(status, "HTTP/1.1 503") {
		t.Fatalf("got %q, want the shedder's 503", status)
	}
}
