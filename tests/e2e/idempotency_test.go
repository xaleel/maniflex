package e2e

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"maniflex"
	"maniflex/middleware/idempotency"
	"maniflex/tests/e2e/testutil"
)

func TestIdempotencyMiddleware(t *testing.T) {
	t.Parallel()

	newSrv := func(t *testing.T) *testutil.Server {
		store := maniflex.NewMemoryCache()
		return testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Deserialize.Register(
					idempotency.Middleware(idempotency.Config{
						Store: store,
						TTL:   24 * time.Hour,
					}),
					maniflex.ForOperation(maniflex.OpCreate),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
	}

	t.Run("replay_same_key_same_body_returns_cached_response", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)

		body := map[string]any{"name": "Ada", "email": "ada@x.com", "password": "s"}
		headers := map[string]string{"Idempotency-Key": "abc-123"}

		first := srv.POST("/users", body, headers).AssertStatus(http.StatusCreated)
		firstID := first.ID()

		second := srv.POST("/users", body, headers).AssertStatus(http.StatusCreated)
		if got := second.ID(); got != firstID {
			t.Fatalf("replay returned id %q, want cached %q", got, firstID)
		}
		if second.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay missing Idempotent-Replayed header: %v", second.Header)
		}
	})

	t.Run("same_key_different_body_returns_422", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		headers := map[string]string{"Idempotency-Key": "key-conflict"}

		srv.POST("/users",
			map[string]any{"name": "A", "email": "a@x.com", "password": "s"},
			headers,
		).AssertStatus(http.StatusCreated)

		resp := srv.POST("/users",
			map[string]any{"name": "B", "email": "b@x.com", "password": "s"},
			headers,
		)
		resp.AssertStatus(http.StatusUnprocessableEntity)
		if got := resp.ErrorCode(); got != "IDEMPOTENCY_KEY_REUSED" {
			t.Fatalf("error code: got %q, want IDEMPOTENCY_KEY_REUSED", got)
		}
	})

	t.Run("missing_key_passes_through", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)

		// Two requests with no Idempotency-Key — second one must hit the unique
		// constraint on email, proving the pipeline ran rather than replaying.
		body := map[string]any{"name": "C", "email": "c@x.com", "password": "s"}
		srv.POST("/users", body).AssertStatus(http.StatusCreated)
		srv.POST("/users", body).AssertStatus(http.StatusConflict)
	})

	t.Run("different_keys_create_distinct_records", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)

		first := srv.POST("/users",
			map[string]any{"name": "D1", "email": "d1@x.com", "password": "s"},
			map[string]string{"Idempotency-Key": "k1"},
		).AssertStatus(http.StatusCreated)

		second := srv.POST("/users",
			map[string]any{"name": "D2", "email": "d2@x.com", "password": "s"},
			map[string]string{"Idempotency-Key": "k2"},
		).AssertStatus(http.StatusCreated)

		if first.ID() == second.ID() {
			t.Fatalf("distinct keys returned identical IDs %q", first.ID())
		}
	})

	t.Run("failed_response_is_not_cached", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		headers := map[string]string{"Idempotency-Key": "retry-me"}

		// Validation failure — missing required fields.
		srv.POST("/users",
			map[string]any{"name": "Only Name"},
			headers,
		).AssertStatus(http.StatusUnprocessableEntity)

		// Same key, valid body — must run the pipeline (not replay the 422).
		srv.POST("/users",
			map[string]any{"name": "Now Valid", "email": "valid@x.com", "password": "s"},
			headers,
		).AssertStatus(http.StatusCreated)
	})

	// Roadmap §11A.10 regression: two concurrent requests with the same
	// Idempotency-Key and identical bodies must result in exactly ONE record,
	// not two. Pre-fix both calls missed the cache, both ran the pipeline,
	// and both inserted — silently breaking the contract that the key
	// uniquely identifies one logical operation. The in-process Locker
	// added by 11A.10 serialises the first-miss so only one pipeline
	// execution wins; the other waits and replays from cache.
	t.Run("concurrent_first_miss_serialises_to_single_pipeline_run", func(t *testing.T) {
		t.Parallel()
		// Use a fixed-KeyFunc Config so concurrent requests share a cache
		// key even though their ephemeral TCP ports (and therefore
		// RemoteAddr) differ.
		store := maniflex.NewMemoryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Deserialize.Register(
					idempotency.Middleware(idempotency.Config{
						Store:   store,
						TTL:     24 * time.Hour,
						KeyFunc: func(ctx *maniflex.ServerContext) string { return "test-scope" },
					}),
					maniflex.ForOperation(maniflex.OpCreate),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})

		headers := map[string]string{"Idempotency-Key": "race-1"}
		body := map[string]any{"name": "Race", "email": "race1@x.com", "password": "s"}

		const n = 6
		var wg sync.WaitGroup
		ids := make([]string, n)
		statuses := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp := srv.POST("/users", body, headers)
				statuses[i] = resp.Status
				if resp.Status == http.StatusCreated {
					ids[i] = resp.ID()
				}
			}(i)
		}
		wg.Wait()

		for i, s := range statuses {
			if s != http.StatusCreated {
				t.Errorf("goroutine %d: got %d, want 201", i, s)
			}
		}
		first := ids[0]
		for i, id := range ids {
			if id != first {
				t.Errorf("goroutine %d: got id %q, want %q (pipeline ran more than once)", i, id, first)
			}
		}

		// Exactly one user row in the DB.
		items := srv.GET("/users?filter=email:eq:race1@x.com").DataList()
		testutil.AssertLen(t, "users with the contested email", items, 1)
	})

	// Custom Locker implementation: the middleware honours it, and a no-op
	// Locker (acquired=true always, no actual serialisation) faithfully
	// reproduces the pre-fix race so callers know the hook is wired.
	t.Run("custom_locker_is_used", func(t *testing.T) {
		t.Parallel()
		var calls int
		var mu sync.Mutex
		lock := &trackingLocker{onAcquire: func() {
			mu.Lock()
			calls++
			mu.Unlock()
		}}

		store := maniflex.NewMemoryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Deserialize.Register(
					idempotency.Middleware(idempotency.Config{
						Store:  store,
						TTL:    24 * time.Hour,
						Locker: lock,
					}),
					maniflex.ForOperation(maniflex.OpCreate),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})

		srv.POST("/users",
			map[string]any{"name": "L1", "email": "l1@x.com", "password": "s"},
			map[string]string{"Idempotency-Key": "lock-1"},
		).AssertStatus(http.StatusCreated)

		mu.Lock()
		got := calls
		mu.Unlock()
		if got != 1 {
			t.Errorf("custom Locker.Acquire called %d times, want 1", got)
		}
	})
}

// trackingLocker is a test-only Locker that records every Acquire call and
// behaves as singleflight (single in-flight per key).
type trackingLocker struct {
	mu        sync.Mutex
	inflight  map[string]chan struct{}
	onAcquire func()
}

func (l *trackingLocker) Acquire(ctx context.Context, key string, _ time.Duration) (bool, func(), error) {
	if l.onAcquire != nil {
		l.onAcquire()
	}
	l.mu.Lock()
	if l.inflight == nil {
		l.inflight = make(map[string]chan struct{})
	}
	if ch, ok := l.inflight[key]; ok {
		l.mu.Unlock()
		<-ch
		return false, nil, nil
	}
	ch := make(chan struct{})
	l.inflight[key] = ch
	l.mu.Unlock()
	return true, func() {
		l.mu.Lock()
		delete(l.inflight, key)
		l.mu.Unlock()
		close(ch)
	}, nil
}
