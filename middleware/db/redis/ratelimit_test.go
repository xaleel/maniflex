package redis

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeCounter records what the backend asked Redis to do.
type fakeCounter struct {
	key    string
	window time.Duration
	calls  int
	ret    int64
	err    error
}

func (f *fakeCounter) IncrExpireNX(_ context.Context, key string, window time.Duration) (int64, error) {
	f.calls++
	f.key, f.window = key, window
	return f.ret, f.err
}

func backendWith(prefix string, f *fakeCounter) *RateLimitBackend {
	return &RateLimitBackend{ops: f, prefix: prefix}
}

func TestIncrement_PrefixesTheKey(t *testing.T) {
	f := &fakeCounter{ret: 1}
	if _, err := backendWith("myapp:ratelimit", f).Increment(context.Background(), "1.2.3.4", time.Minute); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if want := "myapp:ratelimit:1.2.3.4"; f.key != want {
		t.Errorf("counter key = %q, want %q", f.key, want)
	}
}

func TestIncrement_UsesTheBareKeyWhenThePrefixIsEmpty(t *testing.T) {
	f := &fakeCounter{ret: 1}
	if _, err := backendWith("", f).Increment(context.Background(), "1.2.3.4", time.Minute); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if f.key != "1.2.3.4" {
		t.Errorf("counter key = %q, want %q — an empty prefix must not contribute a separator", f.key, "1.2.3.4")
	}
}

func TestIncrement_PassesTheWindowThrough(t *testing.T) {
	f := &fakeCounter{ret: 1}
	if _, err := backendWith("p", f).Increment(context.Background(), "k", 90*time.Second); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if f.window != 90*time.Second {
		t.Errorf("counter window = %v, want 90s — the TTL is what makes the window reset", f.window)
	}
}

func TestIncrement_ReturnsTheCounterValue(t *testing.T) {
	f := &fakeCounter{ret: 7}
	got, err := backendWith("p", f).Increment(context.Background(), "k", time.Minute)
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if got != 7 {
		t.Errorf("Increment = %d, want 7 — the limiter compares this against the configured ceiling", got)
	}
}

func TestIncrement_WrapsTheBackendError(t *testing.T) {
	sentinel := errors.New("dial tcp: connection refused")
	// ret is non-zero so that returning the counter alongside the error is a
	// distinguishable mistake rather than an assertion that cannot fail.
	f := &fakeCounter{ret: 9, err: sentinel}
	got, err := backendWith("p", f).Increment(context.Background(), "k", time.Minute)
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
	if got != 0 {
		t.Errorf("Increment = %d on error, want 0 — a non-zero count would be read as real traffic", got)
	}
}
