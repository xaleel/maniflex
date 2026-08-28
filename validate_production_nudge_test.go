package maniflex

// Audit DX4: ValidateProduction is opt-in and nothing pointed at it. The opt-in
// half is deliberate and said so at production.go:9; the "nothing nudges" half
// was true only outside the docs site — six pages cover it, but nothing in
// godoc or at run time did.
//
// The run-time half is this. Config.Strict defaults to false and its own doc
// says "turn it on in CI and staging", so setting it is a declaration of
// production-ish intent: someone who has done that and never called
// ValidateProduction is exactly the person the audit describes, and a developer
// running with the defaults never sees the line.
//
// It is a warning, not a strict issue. issues.addStrict would fail the boot,
// which would make ValidateProduction mandatory under Strict — a different and
// breaking decision, and one Strict's own doc rules out: it gates "warnings that
// describe something a reasonable application might mean", where
// ValidateProduction is opinionated about migration and query limits.
//
//	go test . -run TestValidateProductionNudge

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type bootLogRecorder struct {
	mu   *sync.Mutex
	recs *[]slog.Record
}

func (h bootLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h bootLogRecorder) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, r)
	return nil
}
func (h bootLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h bootLogRecorder) WithGroup(string) slog.Handler      { return h }

// bootLogs builds a server with cfg, forces the router to be constructed, and
// returns everything logged while that happened.
func bootLogs(t *testing.T, cfg Config, validate bool) []slog.Record {
	t.Helper()
	var mu sync.Mutex
	recs := &[]slog.Record{}
	cfg.Logger = slog.New(bootLogRecorder{mu: &mu, recs: recs})

	s := New(cfg)
	if validate {
		// The error is not the point — an empty server has production issues of
		// its own. What matters is that the call was made.
		_ = s.ValidateProduction()
	}
	_ = s.Handler()

	mu.Lock()
	defer mu.Unlock()
	return append([]slog.Record(nil), *recs...)
}

func mentionsValidateProduction(recs []slog.Record) bool {
	for _, r := range recs {
		if r.Level >= slog.LevelWarn && strings.Contains(r.Message, "ValidateProduction") {
			return true
		}
	}
	return false
}

// The nudge: Strict declares production intent, so a server that never asked
// for the audit should be told the audit exists.
func TestValidateProductionNudge_WarnsUnderStrictWhenNotCalled(t *testing.T) {
	if !mentionsValidateProduction(bootLogs(t, Config{Strict: true}, false)) {
		t.Error("Config.Strict was set and ValidateProduction was never called, and boot said " +
			"nothing: the strictest checks the framework has stay unrun and unmentioned")
	}
}

// Having called it must silence the nudge, or it is not a nudge, it is noise.
func TestValidateProductionNudge_SilentWhenCalled(t *testing.T) {
	if mentionsValidateProduction(bootLogs(t, Config{Strict: true}, true)) {
		t.Error("the nudge fired even though ValidateProduction had been called: an operator " +
			"who did the right thing is told to do it on every boot")
	}
}

// And a developer running with the defaults must never see it.
func TestValidateProductionNudge_SilentWithoutStrict(t *testing.T) {
	if mentionsValidateProduction(bootLogs(t, Config{}, false)) {
		t.Error("the nudge fired on a default (non-strict) server: development is exactly " +
			"where ValidateProduction is deliberately not wanted")
	}
}
