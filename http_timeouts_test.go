package maniflex

// The framework owns the http.Server, and net/http gives that struct no timeouts
// at all: an unconfigured server answers slowloris by holding the connection open
// forever, so enough dribbling clients exhaust its file descriptors without one
// request ever reaching the pipeline. ApplyDefaults now supplies the defensive
// read deadlines, and Config exposes all four (DX-1).

import (
	"testing"
	"time"
)

func TestApplyDefaults_HTTPTimeouts(t *testing.T) {
	var cfg Config
	cfg.ApplyDefaults()

	if cfg.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s — a bare Config must not be slowloris-able", cfg.ReadHeaderTimeout)
	}
	if cfg.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v, want 120s", cfg.IdleTimeout)
	}
	// These two cut off slow uploads and long-lived streams (SSE), so they stay
	// the caller's decision.
	if cfg.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 (unset by default)", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (unset by default)", cfg.WriteTimeout)
	}
}

func TestApplyDefaults_HTTPTimeoutsRespectExplicitValues(t *testing.T) {
	cfg := Config{
		ReadHeaderTimeout: 2 * time.Second,
		IdleTimeout:       5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      45 * time.Second,
	}
	cfg.ApplyDefaults()

	if cfg.ReadHeaderTimeout != 2*time.Second || cfg.IdleTimeout != 5*time.Second {
		t.Errorf("defaults overwrote explicit values: %+v", cfg)
	}
}

func TestNewHTTPServer_CarriesTimeouts(t *testing.T) {
	cfg := Config{
		ReadHeaderTimeout: time.Second,
		IdleTimeout:       2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
	}
	srv := newHTTPServer(":8080", nil, &cfg)

	if srv.ReadHeaderTimeout != time.Second || srv.IdleTimeout != 2*time.Second ||
		srv.ReadTimeout != 3*time.Second || srv.WriteTimeout != 4*time.Second {
		t.Errorf("http.Server did not carry the configured timeouts: %+v", srv)
	}
	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", srv.Addr)
	}
}

// A negative value is the documented way to say "no deadline", which the
// http.Server spells as zero.
func TestNewHTTPServer_NegativeDisables(t *testing.T) {
	cfg := Config{ReadHeaderTimeout: -1, IdleTimeout: -1}
	srv := newHTTPServer(":0", nil, &cfg)

	if srv.ReadHeaderTimeout != 0 {
		t.Errorf("ReadHeaderTimeout = %v, want 0 (disabled)", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 0 {
		t.Errorf("IdleTimeout = %v, want 0 (disabled)", srv.IdleTimeout)
	}
}
