package e2e

// Regression (delivery svc #6): db.RateLimit and db.AuditLog register on the DB
// step, which custom actions skip — so an all-action service got zero rate
// limiting and zero audit records. The *Action variants run in the action's own
// Middleware list (between Auth and the handler) and cover those paths.

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	db "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type sliceAuditSink struct {
	mu      sync.Mutex
	records []db.AuditRecord
	written chan struct{}
}

func (s *sliceAuditSink) Write(_ context.Context, r db.AuditRecord) error {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	select {
	case s.written <- struct{}{}:
	default:
	}
	return nil
}

func (s *sliceAuditSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func TestRateLimitAction_LimitsCustomAction(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/limited",
				Middleware: []maniflex.MiddlewareFunc{
					db.RateLimitAction(db.RateLimitConfig{RequestsPerMinute: 2}),
				},
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	srv.POST("/limited", nil).AssertStatus(http.StatusOK)
	srv.POST("/limited", nil).AssertStatus(http.StatusOK)
	srv.POST("/limited", nil).AssertStatus(http.StatusTooManyRequests)
}

func TestAuditLogAction_RecordsCustomAction(t *testing.T) {
	sink := &sliceAuditSink{written: make(chan struct{}, 4)}
	srv := testutil.NewServer(t, testutil.Options{
		Middleware: func(s *maniflex.Server) {
			// Populate ctx.Auth so the audit record has an actor.
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Auth = &maniflex.AuthInfo{UserID: "actor-1", TenantID: "t-9"}
				return next()
			})
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/audited",
				Middleware: []maniflex.MiddlewareFunc{
					db.AuditLogAction(sink),
				},
				Handler: func(ctx *maniflex.ServerContext) error {
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{"ok": true}}
					return nil
				},
			})
		},
	})

	srv.POST("/audited", nil).AssertStatus(http.StatusOK)
	// Audit writes are fire-and-forget on a background goroutine; wait for it.
	select {
	case <-sink.written:
	case <-time.After(2 * time.Second):
		t.Fatal("audit record was not written within 2s")
	}

	if sink.len() != 1 {
		t.Fatalf("expected 1 audit record, got %d", sink.len())
	}
	rec := sink.records[0]
	if rec.Actor != "actor-1" {
		t.Errorf("audit actor: got %q, want actor-1", rec.Actor)
	}
	if rec.Operation != maniflex.OpAction {
		t.Errorf("audit op: got %q, want OpAction", rec.Operation)
	}
}
