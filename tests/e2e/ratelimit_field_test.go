package e2e_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"maniflex"
	dbmw "maniflex/middleware/db"
	"maniflex/tests/e2e/testutil"
)

// fieldRateLimitBackend is a deterministic in-memory RateLimitBackend for tests.
type fieldRateLimitBackend struct {
	mu     sync.Mutex
	counts map[string]int64
	calls  int
}

func (f *fieldRateLimitBackend) Increment(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.counts[key]++
	return f.counts[key], nil
}

// RateLimitTarget is a model used exclusively for rate-limit-by-field tests.
type RateLimitTarget struct {
	maniflex.BaseModel
	Email   string `json:"email"   db:"email"   mfx:"required,filterable"`
	Payload string `json:"payload" db:"payload" mfx:"required"`
}

func TestRateLimitField_BlocksAfterLimit(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 3, time.Minute),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	body := func() map[string]any {
		return map[string]any{"email": "test@example.com", "payload": "x"}
	}

	// First 3 requests must succeed.
	for i := range 3 {
		s.POST("/rate_limit_targets", body()).AssertStatus(http.StatusCreated)
		_ = i
	}
	// 4th request with the same email must be rate-limited.
	s.POST("/rate_limit_targets", body()).AssertStatus(http.StatusTooManyRequests)
}

func TestRateLimitField_DifferentValuesHaveSeparateBuckets(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 2, time.Minute),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	// Exhaust bucket for alice.
	for range 2 {
		s.POST("/rate_limit_targets", map[string]any{
			"email": "alice@example.com", "payload": "x",
		}).AssertStatus(http.StatusCreated)
	}
	s.POST("/rate_limit_targets", map[string]any{
		"email": "alice@example.com", "payload": "x",
	}).AssertStatus(http.StatusTooManyRequests)

	// Bob's bucket is independent — must still succeed.
	s.POST("/rate_limit_targets", map[string]any{
		"email": "bob@example.com", "payload": "x",
	}).AssertStatus(http.StatusCreated)
}

func TestRateLimitField_MissingFieldSkipsRateLimit(t *testing.T) {
	// Model with an optional email to test the missing-field path.
	type OptionalEmail struct {
		maniflex.BaseModel
		Name  string  `json:"name"  db:"name"  mfx:"required"`
		Email *string `json:"email" db:"email" mfx:"filterable"`
	}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{OptionalEmail{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 1, time.Minute),
				maniflex.ForModel("OptionalEmail"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	// Send many requests without the email field — none should be rate-limited.
	for range 5 {
		s.POST("/optional_emails", map[string]any{"name": "X"}).
			AssertStatus(http.StatusCreated)
	}
}

func TestRateLimitField_Sets429WithRetryAfter(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 1, 2*time.Minute),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	body := map[string]any{"email": "retry@example.com", "payload": "x"}
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)

	resp := s.POST("/rate_limit_targets", body)
	resp.AssertStatus(http.StatusTooManyRequests)
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must include a Retry-After header")
	}
	// Window is 2 minutes = 120 seconds.
	if resp.Header.Get("Retry-After") != "120" {
		t.Errorf("Retry-After: got %q, want %q", resp.Header.Get("Retry-After"), "120")
	}
}

func TestRateLimitField_WithCustomBackend(t *testing.T) {
	backend := &fieldRateLimitBackend{counts: map[string]int64{}}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 2, time.Minute,
					dbmw.WithRateLimitBackend(backend),
				),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	body := map[string]any{"email": "shared@example.com", "payload": "x"}
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusTooManyRequests)

	backend.mu.Lock()
	calls := backend.calls
	backend.mu.Unlock()
	if calls < 3 {
		t.Errorf("backend Increment must be called at least 3 times, got %d", calls)
	}
}

func TestRateLimitField_WithCustomErrorMessage(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 1, time.Minute,
					dbmw.WithRateLimitErrorMessage("too many password reset attempts"),
				),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	body := map[string]any{"email": "msg@example.com", "payload": "x"}
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)

	resp := s.POST("/rate_limit_targets", body)
	resp.AssertStatus(http.StatusTooManyRequests)
	resp.AssertJSON(func(b map[string]any) {
		errObj, _ := b["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if msg != "too many password reset attempts" {
			t.Errorf("error message: got %q, want custom message", msg)
		}
	})
}

func TestRateLimitField_WindowDuration(t *testing.T) {
	// Window-based config: use a short window so we can verify the field
	// (not just that it compiles). Actual expiry is not tested since it
	// would require sleeping; we verify the limit fires correctly.
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{RateLimitTarget{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.RateLimitField("email", 2, 30*time.Second),
				maniflex.ForModel("RateLimitTarget"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	body := map[string]any{"email": "win@example.com", "payload": "x"}
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusCreated)
	s.POST("/rate_limit_targets", body).AssertStatus(http.StatusTooManyRequests)
}
