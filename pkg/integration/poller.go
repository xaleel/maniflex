package integration

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Poller invokes Fn on a fixed interval until the supplied context is
// cancelled. Errors from Fn are logged via the configured Logger (default
// slog.Default) and the loop continues — Poller is intended for best-effort
// background work where one failed tick should not stop the schedule.
//
// Example: pulling new fingerprint records from a wall terminal every 30
// seconds.
//
//	p := &integration.Poller{
//	    Interval: 30 * time.Second,
//	    Fn: func(ctx context.Context) error {
//	        return terminal.Sync(ctx)
//	    },
//	}
//	go p.Start(ctx)
type Poller struct {
	// Interval between tick starts. A tick that runs longer than Interval
	// delays the next one — there is no overlap.
	Interval time.Duration

	// Fn is the work performed on each tick. Honour ctx — Poller cancels it
	// when Start's context is cancelled.
	Fn func(ctx context.Context) error

	// RunOnStart fires Fn immediately when Start is called, before the first
	// interval. Default false: the first tick happens after Interval.
	RunOnStart bool

	// Logger captures per-tick errors. Default slog.Default().
	Logger *slog.Logger
}

// Start blocks until ctx is cancelled, invoking Fn on every Interval (and
// optionally immediately when RunOnStart is true). Returns ctx.Err() once
// the loop exits, or an error when configuration is invalid.
func (p *Poller) Start(ctx context.Context) error {
	if p.Interval <= 0 {
		return errors.New("integration: Poller.Interval must be > 0")
	}
	if p.Fn == nil {
		return errors.New("integration: Poller.Fn is nil")
	}
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}

	tick := func() {
		if err := p.Fn(ctx); err != nil && !isContextErr(err) {
			logger.Warn("integration.Poller tick failed", "error", err.Error())
		}
	}

	if p.RunOnStart {
		tick()
	}

	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			tick()
		}
	}
}
