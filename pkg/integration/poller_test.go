package integration

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoller_TicksOnInterval(t *testing.T) {
	var calls int32
	p := &Poller{
		Interval: 10 * time.Millisecond,
		Fn: func(ctx context.Context) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = p.Start(ctx)
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Errorf("expected at least 2 ticks in 50ms@10ms, got %d", got)
	}
}

func TestPoller_RunOnStart(t *testing.T) {
	var calls int32
	p := &Poller{
		Interval:   time.Second, // long; we should still see one immediate call
		RunOnStart: true,
		Fn: func(ctx context.Context) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = p.Start(ctx)
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 immediate call (no further ticks within 30ms), got %d", calls)
	}
}

func TestPoller_ContinuesAfterError(t *testing.T) {
	var calls int32
	p := &Poller{
		Interval: 10 * time.Millisecond,
		Fn: func(ctx context.Context) error {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return errors.New("first tick fails")
			}
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), // silent
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = p.Start(ctx)
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("loop should continue past first error; got %d calls", calls)
	}
}

func TestPoller_StopsOnContextCancel(t *testing.T) {
	var calls int32
	p := &Poller{
		Interval: time.Millisecond,
		Fn: func(ctx context.Context) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(15 * time.Millisecond)
		cancel()
	}()
	err := p.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Start should return ctx.Err(); got %v", err)
	}
}

func TestPoller_InvalidIntervalErrors(t *testing.T) {
	p := &Poller{
		Interval: 0,
		Fn:       func(ctx context.Context) error { return nil },
	}
	if err := p.Start(context.Background()); err == nil {
		t.Error("Interval=0 should error")
	}
}

func TestPoller_NilFnErrors(t *testing.T) {
	p := &Poller{Interval: time.Second}
	if err := p.Start(context.Background()); err == nil {
		t.Error("Fn=nil should error")
	}
}
