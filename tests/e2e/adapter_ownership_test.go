package e2e

// Audit HTTP-10 — shutdown.md step 6 said the database adapter's Close() is
// called when the drain finishes. Nothing in core has ever called it: the
// adapter belongs to whoever opened it, which is what lets a jobs queue or the
// admin panel share the pool. A reader who trusted that step would drop the
// `defer db.Close()` every example has and leak the pool in any process that
// outlives the server.
//
// The doc now states the ownership rule, and this pins it: shutdown must leave
// the pool open, and closing it afterwards must still reach the adapter.

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// closeCountingAdapter forwards every call to the real adapter and records whether
// anyone closed it.
type closeCountingAdapter struct {
	maniflex.DBAdapter
	closes atomic.Int64
}

func (a *closeCountingAdapter) Close() error {
	a.closes.Add(1)
	return a.DBAdapter.Close()
}

func TestAdapterOwnership_ShutdownDoesNotCloseTheAdapter(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	server := maniflex.New(maniflex.Config{
		Port:            port,
		PathPrefix:      "/api",
		ShutdownTimeout: 3 * time.Second,
	})
	server.MustRegister(testutil.DefaultModels()...)

	inner, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatal(err)
	}
	db := &closeCountingAdapter{DBAdapter: inner}
	t.Cleanup(func() { db.DBAdapter.Close() })
	server.SetDB(db)

	done := make(chan error, 1)
	go func() { done <- server.Start() }()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/api/health"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}

	if n := db.closes.Load(); n != 0 {
		t.Fatalf("shutdown called the adapter's Close %d times; the pool is the "+
			"caller's, and anything else sharing it would lose its connection", n)
	}

	// The other half of the rule: the documented close-after-Start still works.
	if err := db.Close(); err != nil {
		t.Fatalf("closing the adapter after shutdown: %v", err)
	}
	if n := db.closes.Load(); n != 1 {
		t.Fatalf("Close reached the adapter %d times, want 1", n)
	}
}
