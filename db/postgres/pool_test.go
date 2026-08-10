package postgres

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// ── Pool defaults ─────────────────────────────────────────────────────────────

func TestPoolDefaultsAreSmallEnoughForEntryTierServers(t *testing.T) {
	t.Parallel()

	write := PoolConfig{}.withDefaults(true)
	read := PoolConfig{}.withDefaults(false)

	if write.MaxOpenConns != 3 {
		t.Errorf("write MaxOpenConns = %d, want 3", write.MaxOpenConns)
	}
	if read.MaxOpenConns != 6 {
		t.Errorf("read MaxOpenConns = %d, want 6", read.MaxOpenConns)
	}

	// The ceiling that matters is the sum: Open always builds both pools, even
	// when readDSN is empty and both point at the same primary. Two processes
	// must still fit the smallest managed tier on the market (Heroku Postgres
	// Essential, 20 connections) with room left for psql and a migration job.
	total := write.MaxOpenConns + read.MaxOpenConns
	if got := total * 2; got > 20 {
		t.Errorf("two processes take %d connections, want ≤ 20 so they fit an "+
			"entry-tier server (per-process total is %d)", got, total)
	}
}

func TestPoolDefaultsKeepEveryConnectionIdle(t *testing.T) {
	t.Parallel()

	// Half of a 3-connection pool is 1, so a burst would close two connections
	// the moment they were returned and reopen them on the next one — a TLS
	// handshake plus the session SET round trip each time. A pool this small
	// keeps all of its connections; ConnMaxIdleTime is what reaps them.
	for _, isWriter := range []bool{true, false} {
		got := PoolConfig{}.withDefaults(isWriter)
		if got.MaxIdleConns != got.MaxOpenConns {
			t.Errorf("isWriter=%v: MaxIdleConns = %d, want %d (= MaxOpenConns)",
				isWriter, got.MaxIdleConns, got.MaxOpenConns)
		}
	}
}

func TestPoolDefaultsLeaveExplicitValuesAlone(t *testing.T) {
	t.Parallel()

	got := PoolConfig{
		MaxOpenConns:    50,
		MaxIdleConns:    7,
		ConnMaxLifetime: time.Minute,
		ConnMaxIdleTime: time.Second,
	}.withDefaults(true)

	if got.MaxOpenConns != 50 || got.MaxIdleConns != 7 ||
		got.ConnMaxLifetime != time.Minute || got.ConnMaxIdleTime != time.Second {
		t.Errorf("withDefaults overwrote explicit values: %+v", got)
	}
}

func TestPoolDefaultsKeepLifetimes(t *testing.T) {
	t.Parallel()

	got := PoolConfig{}.withDefaults(true)
	if got.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("ConnMaxLifetime = %v, want 30m", got.ConnMaxLifetime)
	}
	if got.ConnMaxIdleTime != 5*time.Minute {
		t.Errorf("ConnMaxIdleTime = %v, want 5m", got.ConnMaxIdleTime)
	}
}

// ── Capacity check ────────────────────────────────────────────────────────────

// capturingHandler records the slog records written to it.
type capturingHandler struct {
	records []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) attr(i int, key string) (slog.Value, bool) {
	var v slog.Value
	var found bool
	h.records[i].Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value, true
			return false
		}
		return true
	})
	return v, found
}

func serverMax(n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return n, nil }
}

func TestCheckPoolCapacityWarnsWhenPoolsClaimMoreThanHalfTheServer(t *testing.T) {
	t.Parallel()

	h := &capturingHandler{}
	// 9 of a 12-connection server: one more process and the server is full.
	checkPoolCapacity(context.Background(), slog.New(h), 3, 6, serverMax(12))

	if len(h.records) != 1 {
		t.Fatalf("logged %d records, want 1", len(h.records))
	}
	if lvl := h.records[0].Level; lvl != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", lvl)
	}
	for _, want := range []struct {
		key string
		val int64
	}{
		{"pool_max", 9},
		{"write_max", 3},
		{"read_max", 6},
		{"server_max_connections", 12},
	} {
		got, ok := h.attr(0, want.key)
		if !ok {
			t.Errorf("warning has no %q attribute", want.key)
			continue
		}
		if got.Int64() != want.val {
			t.Errorf("%s = %d, want %d", want.key, got.Int64(), want.val)
		}
	}
}

func TestCheckPoolCapacityIsSilentWhenPoolsFitComfortably(t *testing.T) {
	t.Parallel()

	h := &capturingHandler{}
	// The shipped defaults against DigitalOcean's smallest cluster.
	checkPoolCapacity(context.Background(), slog.New(h), 3, 6, serverMax(25))

	if len(h.records) != 0 {
		t.Errorf("logged %d records, want none: %v", len(h.records), h.records)
	}
}

func TestCheckPoolCapacityIsSilentAtExactlyHalf(t *testing.T) {
	t.Parallel()

	h := &capturingHandler{}
	checkPoolCapacity(context.Background(), slog.New(h), 3, 7, serverMax(20))

	if len(h.records) != 0 {
		t.Errorf("10 of 20 warned; the warning is for exceeding half, not reaching it")
	}
}

func TestCheckPoolCapacityStaysSilentWhenTheServerCannotBeAsked(t *testing.T) {
	t.Parallel()

	h := &capturingHandler{}
	failed := func(context.Context) (int, error) {
		return 0, errors.New("permission denied for function current_setting")
	}
	checkPoolCapacity(context.Background(), slog.New(h), 30, 60, failed)

	if len(h.records) != 0 {
		t.Errorf("a failed capacity probe logged %d records, want none — the probe "+
			"is a diagnostic and must stay quiet when it cannot answer", len(h.records))
	}
}

func TestCheckPoolCapacityStaysSilentOnNonsenseServerLimit(t *testing.T) {
	t.Parallel()

	h := &capturingHandler{}
	checkPoolCapacity(context.Background(), slog.New(h), 30, 60, serverMax(0))

	if len(h.records) != 0 {
		t.Errorf("a zero max_connections warned; it means the answer was unusable")
	}
}

// ── SessionConfig.Logger ──────────────────────────────────────────────────────

func TestSessionConfigLoggerDefaultsToSlogDefault(t *testing.T) {
	t.Parallel()

	if got := (SessionConfig{}).log(); got != slog.Default() {
		t.Errorf("log() = %v, want slog.Default()", got)
	}
}

func TestSessionConfigLoggerIsUsedWhenSet(t *testing.T) {
	t.Parallel()

	want := slog.New(&capturingHandler{})
	if got := (SessionConfig{Logger: want}).log(); got != want {
		t.Errorf("log() = %v, want the configured logger", got)
	}
}
