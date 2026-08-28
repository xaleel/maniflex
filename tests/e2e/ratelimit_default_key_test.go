package e2e_test

// The default RateLimit key for an unauthenticated request was
// ctx.Request.RemoteAddr, which Go populates as "IP:port" — the port being the
// client's ephemeral one, different on every TCP connection. So each connection
// got its own bucket and the limit never fired: ten requests from one address
// produced ten keys (127.0.0.1:58360 … :58369, measured).
//
// It was known in effect if not in name — proxy_ip_test.go's ipKeyRateLimit
// writes its own KeyFunc "dropping the ephemeral TCP port so the bucket is
// stable", and middleware/auth/read_audit.go already SplitHostPorts before
// recording an IP. Only the default was left keying on the port.
//
// This matters most where IP keying is the only defence there is: an
// unauthenticated endpoint. db.RateLimit's own doc example is
// ForModel("PasswordReset").
//
//	go test ./tests/e2e/ -run TestRateLimitDefaultKey

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// defaultKeyRateLimit registers RateLimit with no KeyFunc, so the built-in key
// is what is under test.
func defaultKeyRateLimit(srv *maniflex.Server, perMinute int) {
	srv.Pipeline.DB.Register(
		dbmw.RateLimit(dbmw.RateLimitConfig{RequestsPerMinute: perMinute}),
		maniflex.ForModel("ProxyTarget"),
		maniflex.ForOperation(maniflex.OpCreate),
	)
}

// postOnFreshConnection sends one request over a connection of its own, so the
// source port differs every time — what a client that does not pool looks like,
// and what anyone hammering an endpoint looks like.
func postOnFreshConnection(t *testing.T, s *testutil.Server, headers map[string]string) int {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer c.CloseIdleConnections()

	req, err := http.NewRequest("POST", s.APIPath("/proxy_targets"),
		bytes.NewReader([]byte(`{"payload":"x"}`)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()
	return res.StatusCode
}

// The finding: one client, one address, a new connection each time. The limit
// has to hold, or per-IP limiting is defeated by not reusing a socket.
func TestRateLimitDefaultKey_HoldsAcrossConnections(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models:     []any{ProxyTarget{}},
		Middleware: func(srv *maniflex.Server) { defaultKeyRateLimit(srv, 3) },
	})

	var got []int
	for range 5 {
		got = append(got, postOnFreshConnection(t, s, nil))
	}

	want := []int{
		http.StatusCreated, http.StatusCreated, http.StatusCreated,
		http.StatusTooManyRequests, http.StatusTooManyRequests,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses %v, want %v — every request opened its own connection, so "+
				"if the ephemeral port is part of the key each one gets a bucket to itself "+
				"and the limit never applies", got, want)
		}
	}
}

// Anti-vacuity: the limit must still be per-address, not global. A different
// client must not inherit the exhausted bucket.
func TestRateLimitDefaultKey_SeparatesDistinctClients(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models:            []any{ProxyTarget{}},
		TrustProxyHeaders: true,
		Middleware:        func(srv *maniflex.Server) { defaultKeyRateLimit(srv, 3) },
	})

	// Proxy resolution rewrites RemoteAddr to a bare IP with no port at all, so
	// this also covers the fallback when there is nothing to split.
	for range 3 {
		if code := postOnFreshConnection(t, s, map[string]string{"X-Forwarded-For": "10.0.0.1"}); code != http.StatusCreated {
			t.Fatalf("10.0.0.1 within its limit got %d", code)
		}
	}
	if code := postOnFreshConnection(t, s, map[string]string{"X-Forwarded-For": "10.0.0.1"}); code != http.StatusTooManyRequests {
		t.Errorf("10.0.0.1 over its limit got %d, want 429", code)
	}
	if code := postOnFreshConnection(t, s, map[string]string{"X-Forwarded-For": "10.0.0.2"}); code != http.StatusCreated {
		t.Errorf("10.0.0.2 got %d on its first request, want 201: buckets are no longer "+
			"per-address, so one noisy client now limits everyone", code)
	}
}
