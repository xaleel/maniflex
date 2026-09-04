package maniflex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// shedClient bounds how long a request may take, so a limiter that queues
// instead of shedding fails these tests in seconds rather than hanging until
// go test's own timeout. Blocking is the plausible regression here, and a hang
// is the least useful way to report it.
var shedClient = &http.Client{Timeout: 3 * time.Second}

// blockingHandler holds each request until release is closed, so a test can
// pin slots open and observe what the next request is told.
func blockingHandler(release <-chan struct{}, entered chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
}

func TestConcurrencyLimit_RefusesWhenEverySlotIsTaken(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(concurrencyLimit(1, nil)(blockingHandler(release, entered)))
	// Registered first, so it runs last: httptest.Server.Close waits for
	// outstanding requests, and the held one only returns once release closes.
	// Without this ordering a failed assertion below wedges the teardown
	// instead of reporting, which is how a queueing limiter turned into a hang
	// rather than a failure.
	t.Cleanup(srv.Close)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	go func() {
		resp, err := http.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered // the only slot is now held

	resp, err := shedClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("second request never returned (%v) — the limiter queued it instead of shedding it", err)
	}
	defer resp.Body.Close()
	releaseOnce()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("Retry-After is absent; a shed request needs to be told when to come back")
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "SERVER_BUSY" {
		t.Errorf("error code = %q, want SERVER_BUSY", body.Error.Code)
	}
}

func TestConcurrencyLimit_RefusesBeforeTheHandlerRuns(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var reached atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		blockingHandler(release, entered).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(concurrencyLimit(1, nil)(inner))
	t.Cleanup(srv.Close)
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	go func() {
		resp, err := http.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered

	resp, err := shedClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("second request never returned (%v) — the limiter queued it instead of shedding it", err)
	}
	resp.Body.Close()
	releaseOnce()

	if got := reached.Load(); got != 1 {
		t.Errorf("handler ran %d times, want 1 — the refusal must happen before the pipeline and its DB read", got)
	}
}

func TestConcurrencyLimit_ReleasesTheSlotWhenTheRequestReturns(t *testing.T) {
	srv := httptest.NewServer(concurrencyLimit(1, nil)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	t.Cleanup(srv.Close)

	for i := range 3 {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 — the slot was not released", i, resp.StatusCode)
		}
	}
}

func TestConcurrencyLimit_ReleasesTheSlotWhenTheHandlerPanics(t *testing.T) {
	var panicked atomic.Bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if panicked.CompareAndSwap(false, true) {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(concurrencyLimit(1, nil)(inner))
	t.Cleanup(srv.Close)

	// The panicking request is answered by net/http's own recovery; what matters
	// is that its slot comes back. A leaked slot on a panic would shrink the
	// server's capacity by one every time a handler blew up, until it served
	// nothing but 503s.
	if resp, err := http.Get(srv.URL); err == nil {
		resp.Body.Close()
	}

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request after panic: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — the panicking request leaked its slot", resp.StatusCode)
	}
}

func TestValidateProduction_RequiresAConcurrencyCap(t *testing.T) {
	cfg := productionConfig()
	cfg.MaxConcurrentRequests = 0
	srv := New(cfg)

	err := srv.ValidateProduction()
	if err == nil {
		t.Fatal("production validation accepted an unbounded in-flight request count")
	}
	if !strings.Contains(err.Error(), "Config.MaxConcurrentRequests") {
		t.Errorf("error does not name the field:\n%s", err.Error())
	}

	srv = New(productionConfig())
	if err := srv.ValidateProduction(); err != nil && strings.Contains(err.Error(), "MaxConcurrentRequests") {
		t.Errorf("a configured cap is still reported:\n%s", err.Error())
	}
}

// probeHoldModel gives the server a route that can hold the only slot.
type probeHoldModel struct {
	BaseModel
	Title string `json:"title"`
}

// A pod that is merely busy must not report itself dead or unfit for traffic. A
// shed /live is read by Kubernetes as a container to restart, and a shed /ready
// as a pod to remove from the Service — and because every replica saturates at
// the same moment, a spike the shedder was absorbing empties the endpoint list
// instead (audit HTTP-3).
func TestConcurrencyLimit_ProbesAreNotShed(t *testing.T) {
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
	srv := New(Config{MaxConcurrentRequests: 1, HTTPMiddlewares: []HTTPMiddleware{hold}})
	srv.MustRegister(probeHoldModel{})
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	ts := httptest.NewServer(h)
	releaseOnce := sync.OnceFunc(func() { close(gate) })
	// Registered first so it runs last: Close waits for the held request.
	t.Cleanup(ts.Close)
	t.Cleanup(releaseOnce)

	go http.Get(ts.URL + "/api/probe_hold_models?hold=1") //nolint:errcheck // released at cleanup
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slot was never occupied; the limiter is not shedding")
	}

	// The slot is taken, so an ordinary route is shed — that is the control.
	resp, err := shedClient.Get(ts.URL + "/api/probe_hold_models")
	if err != nil {
		t.Fatalf("control request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("control request got %d, want 503; the slot is not actually held", resp.StatusCode)
	}

	for _, probe := range []string{"/api/live", "/api/ready", "/api/health"} {
		resp, err := shedClient.Get(ts.URL + probe)
		if err != nil {
			t.Fatalf("GET %s: %v", probe, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			t.Errorf("GET %s answered 503 while the server was merely busy", probe)
		}
	}
}

// The exemption follows Config.PathPrefix and skips a disabled probe, whose path
// is not mounted at all and so has nothing to exempt.
func TestMountedProbePaths(t *testing.T) {
	cfg := Config{PathPrefix: "/v1", Probes: ProbesConfig{Health: ProbeConfig{Disabled: true}}}
	got := mountedProbePaths(&cfg)

	for _, want := range []string{"/v1/live", "/v1/ready"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q is not exempt; probe paths must follow PathPrefix", want)
		}
	}
	if _, ok := got["/v1/health"]; ok {
		t.Error("/v1/health is exempt although the probe is disabled")
	}
}
