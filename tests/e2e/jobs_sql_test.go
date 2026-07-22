package e2e

// Coverage for jobs/sql DEC-11: WithTableName (two queues share one DB),
// WithPayloadCipher (payload encrypted at rest), and the worker requeuing job
// types it doesn't handle instead of dead-lettering them.

import (
	stdsql "database/sql"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func rawJobsDB(t *testing.T) *stdsql.DB {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{testutil.User{}}})
	a, ok := srv.ManiflexServer().DB().(*sqlcore.Adapter)
	if !ok {
		t.Fatalf("expected *sqlcore.Adapter, got %T", srv.ManiflexServer().DB())
	}
	return a.WriteDB()
}

// xorCipher is a trivial reversible cipher for tests only.
type xorCipher struct{}

func xorBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0x5A
	}
	return out
}
func (xorCipher) Encrypt(b []byte) ([]byte, error) { return xorBytes(b), nil }
func (xorCipher) Decrypt(b []byte) ([]byte, error) { return xorBytes(b), nil }

func TestJobsSQL_WithTableName(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()

	if err := jobssql.Migrate(ctx, db, jobsDriver(), jobssql.WithTableName("otp_jobs")); err != nil {
		t.Fatalf("migrate otp_jobs: %v", err)
	}
	q := jobssql.New(db, jobssql.WithTableName("otp_jobs"))

	id, err := q.Enqueue(ctx, jobs.Job{Type: "otp.send", Payload: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, ph(`SELECT COUNT(*) FROM otp_jobs WHERE id=?`), id).Scan(&n); err != nil {
		t.Fatalf("count otp_jobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("otp_jobs should hold the job, got %d rows", n)
	}

	got, err := q.Dequeue(ctx, 1)
	if err != nil || len(got) != 1 || got[0].Type != "otp.send" {
		t.Fatalf("dequeue from custom table: got %v err %v", got, err)
	}
}

func TestJobsSQL_PayloadCipher(t *testing.T) {
	db := rawJobsDB(t)
	ctx := context.Background()

	if err := jobssql.Migrate(ctx, db, jobsDriver()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	q := jobssql.New(db, jobssql.WithPayloadCipher(xorCipher{}))

	id, err := q.Enqueue(ctx, jobs.Job{Type: "email.send", Payload: json.RawMessage(`{"secret":"hunter2"}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx, ph(`SELECT payload FROM job_queue WHERE id=?`), id).Scan(&stored); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.HasPrefix(stored, "encq:") {
		t.Fatalf("payload should be encrypted at rest, got %q", stored)
	}
	if strings.Contains(stored, "hunter2") {
		t.Fatal("plaintext secret leaked into the payload column")
	}

	got, err := q.Dequeue(ctx, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("dequeue: got %v err %v", got, err)
	}
	if string(got[0].Payload) != `{"secret":"hunter2"}` {
		t.Fatalf("payload not decrypted on read: %s", got[0].Payload)
	}
}

// recordingSource hands out one job once, then records whether the worker
// Nacks (requeues) or Deads it.
type recordingSource struct {
	mu           sync.Mutex
	handed       bool
	nacked       []string
	deaded       []string
	nackedSignal chan struct{}
}

func (s *recordingSource) Dequeue(_ context.Context, _ int) ([]jobs.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handed {
		return nil, nil
	}
	s.handed = true
	return []jobs.Job{{ID: "j1", Type: "unknown.type", MaxRetry: 3}}, nil
}
func (s *recordingSource) Ack(context.Context, string) error { return nil }
func (s *recordingSource) Nack(_ context.Context, id string, _ error, _ time.Duration) error {
	s.mu.Lock()
	s.nacked = append(s.nacked, id)
	s.mu.Unlock()
	select {
	case s.nackedSignal <- struct{}{}:
	default:
	}
	return nil
}
func (s *recordingSource) Dead(_ context.Context, id string, _ error) error {
	s.mu.Lock()
	s.deaded = append(s.deaded, id)
	s.mu.Unlock()
	return nil
}

func TestWorker_RequeuesUnhandledType(t *testing.T) {
	src := &recordingSource{nackedSignal: make(chan struct{}, 1)}
	w, err := jobs.NewWorker(jobs.WorkerConfig{
		Source:            src,
		Concurrency:       1,
		EmptyQueueBackoff: 10 * time.Millisecond,
		Handlers: map[string]jobs.Handler{
			// Handles a different type — the dequeued job is "unhandled".
			"known.type": func(context.Context, jobs.Job) (jobs.Result, error) { return jobs.Result{}, nil },
		},
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(runCtx); close(done) }()

	select {
	case <-src.nackedSignal:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("worker did not requeue (Nack) the unhandled job within 3s")
	}
	cancel()
	<-done

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.deaded) != 0 {
		t.Fatalf("unhandled job type was dead-lettered (%v); want requeued via Nack", src.deaded)
	}
	if len(src.nacked) != 1 || src.nacked[0] != "j1" {
		t.Fatalf("expected the unhandled job to be Nacked once, got %v", src.nacked)
	}
}
