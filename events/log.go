package events

import (
	"context"
	"log/slog"
)

// Log returns a Handler that logs each event at INFO level.
// Useful for debugging and as a reference subscriber implementation.
//
//	bus.Subscribe(ctx, events.Subscription{
//	    Patterns: []string{"*"},
//	    Handler:  events.Log(slog.Default()),
//	})
func Log(logger *slog.Logger) Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, e Event) error {
		logger.InfoContext(ctx, "event",
			slog.String("id", e.ID),
			slog.String("type", e.Type),
			slog.String("source", e.Source),
			slog.String("subject", e.Subject),
			slog.String("model", e.Model),
			slog.String("operation", string(e.Operation)),
			slog.String("actor_id", e.ActorID),
			slog.String("trace_id", e.TraceID),
		)
		return nil
	}
}
