// Package jobsmaniflex (imported as jobs/maniflex) integrates the jobs package with the
// maniflex model layer (3C.4). It registers a StatusModel whose rows mirror job
// lifecycle state and exposes them through the standard REST endpoints with
// auth, filtering, pagination, and the existing audit log.
//
// Typical wiring:
//
//	sink, queue, err := jobsmaniflex.Mount(server, rawQueue)
//	if err != nil { log.Fatal(err) }
//	w, _ := jobs.NewWorker(jobs.WorkerConfig{
//	    Source:   queue.(jobs.Source),
//	    Handlers: handlers,
//	    Status:   sink,
//	})
//
// The returned queue wraps rawQueue: Enqueue/EnqueueAt/EnqueueBatch create an
// "enqueued" status row so callers can poll /job_statuses/:id immediately.
// When the inner queue implements jobs.Cancellable, Cancel also updates the row.
package jobsmaniflex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	maniflex "github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/jobs"
)

// writeBlocker rejects external create/update/delete requests on the StatusModel.
func writeBlocker(ctx *maniflex.ServerContext, next func() error) error {
	switch ctx.Operation {
	case maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete:
		ctx.Abort(http.StatusMethodNotAllowed, "JOB_STATUS_READONLY",
			"job status records are managed by the job worker and cannot be modified via the API")
		return nil
	}
	return next()
}

// makeForceFilter returns a middleware that restricts list/read to the caller's
// own actor_id (and tenant_id) unless they hold adminRole.
//
// The filters are marked Forced: it runs on the Auth step, which is before the
// Deserialize step that builds ctx.Query from the request, and only a Forced
// filter is carried across that rebuild — a plain one would be discarded, which
// is exactly what left this scope inert until v0.2.3 (P1-18). Forced is also the
// correct label regardless: this is a scope the server imposes, not a filter the
// client chose.
func makeForceFilter(adminRole string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.HasRole(adminRole) {
			return next()
		}
		// Fail closed. This used to be `|| ctx.Auth == nil`, which skipped the scope
		// entirely for an unauthenticated caller — so wherever these routes were
		// reachable without auth, a list returned every actor's and every tenant's
		// job metadata (audit JB-12). There is no actor to scope to without an
		// identity, and the only safe reading of "no identity" on a per-actor
		// resource is refusal, not an unscoped read. HasRole is nil-safe, so an
		// anonymous caller reaches here rather than passing as an admin.
		if ctx.Auth == nil {
			ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED",
				"job status records are scoped to the authenticated caller")
			return nil
		}
		if ctx.Query == nil {
			ctx.Query = &maniflex.QueryParams{Page: 1, Limit: 20}
		}
		ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
			Field:    "actor_id",
			Operator: maniflex.OpEq,
			Value:    ctx.Auth.UserID,
			Forced:   true,
		})
		if ctx.Auth.TenantID != "" {
			ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
				Field:    "tenant_id",
				Operator: maniflex.OpEq,
				Value:    ctx.Auth.TenantID,
				Forced:   true,
			})
		}
		return next()
	}
}

// StatusModel is a registered Maniflex model whose rows mirror job lifecycle state.
// List/read endpoints are available at /api/job_statuses and
// /api/job_statuses/{id}. Write endpoints (POST/PATCH/DELETE) are blocked by
// Mount's write-blocker middleware.
type StatusModel struct {
	maniflex.BaseModel
	Type        string     `json:"type"         db:"type"         mfx:"filterable,required,immutable"`
	Status      string     `json:"status"       db:"status"       mfx:"filterable,sortable,enum:enqueued|running|succeeded|failed|dead|cancelled"`
	ActorID     string     `json:"actor_id"     db:"actor_id"     mfx:"filterable,immutable"`
	TenantID    string     `json:"tenant_id"    db:"tenant_id"    mfx:"filterable,immutable"`
	Attempts    int        `json:"attempts"     db:"attempts"`
	Error       string     `json:"error"        db:"error"        mfx:"hidden"`
	ResultURL   string     `json:"result_url"   db:"result_url"`
	ResultMime  string     `json:"result_mime"  db:"result_mime"`
	StartedAt   *time.Time `json:"started_at"   db:"started_at"   mfx:"readonly,filterable,sortable"`
	CompletedAt *time.Time `json:"completed_at" db:"completed_at" mfx:"readonly,filterable,sortable"`
}

// MountOptions configures the StatusModel registration.
type MountOptions struct {
	// TableName overrides the default "job_statuses".
	TableName string

	// AdminRole is the role that bypasses the per-actor force-filter.
	// Default: "admin".
	AdminRole string
}

// Mount registers StatusModel with server, installs the write-blocker and
// per-actor force-filter middlewares, and returns a StatusSink and a wrapped Queue.
//
// The returned StatusSink must be passed to jobs.WorkerConfig.Status.
// The returned Queue wraps q: each Enqueue/EnqueueAt/EnqueueBatch call creates an
// "enqueued" status row so clients can poll /job_statuses/:id immediately after
// enqueueing. When q implements jobs.Cancellable, Cancel updates the row to
// "cancelled". When q implements jobs.BlockingSource, the returned queue also
// advertises that interface so the worker can use it.
//
// If server.DB() is non-nil at call time (e.g. when called from testutil's
// Middleware hook after SetDB has run), AutoMigrate is called immediately so the
// job_statuses table exists. In the normal production flow — Mount before SetDB —
// AutoMigrate is deferred to server.Start().
func Mount(server *maniflex.Server, q jobs.Queue, opts ...MountOptions) (jobs.StatusSink, jobs.Queue, error) {
	if q == nil {
		return nil, nil, fmt.Errorf("jobs/maniflex: queue must not be nil")
	}

	opt := MountOptions{TableName: "job_statuses", AdminRole: "admin"}
	if len(opts) > 0 {
		if opts[0].TableName != "" {
			opt.TableName = opts[0].TableName
		}
		if opts[0].AdminRole != "" {
			opt.AdminRole = opts[0].AdminRole
		}
	}
	if err := server.Register(StatusModel{}, maniflex.ModelConfig{
		TableName: opt.TableName,
		Middleware: &maniflex.ModelMiddleware{
			Validate: []maniflex.MiddlewareFunc{writeBlocker},
			Auth:     []maniflex.MiddlewareFunc{makeForceFilter(opt.AdminRole)},
		},
	}); err != nil {
		return nil, nil, fmt.Errorf("jobs/maniflex: register StatusModel: %w", err)
	}

	meta, ok := server.Registry().Get("StatusModel")
	if !ok {
		return nil, nil, fmt.Errorf("jobs/maniflex: StatusModel not in registry after Register")
	}

	// When the DB is already connected (e.g. called after testutil.SetDB), run
	// AutoMigrate now so the job_statuses table is created immediately. In the
	// normal production flow (Mount → SetDB → Start) AutoMigrate runs in Start.
	if db := server.DB(); db != nil {
		if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
			return nil, nil, fmt.Errorf("jobs/maniflex: auto-migrate: %w", err)
		}
	}

	sink := &maniflexStatusSink{srv: server, meta: meta}

	sq := &statusQueue{inner: q, sink: sink}
	var wrapped jobs.Queue = sq
	if _, ok := q.(jobs.BlockingSource); ok {
		wrapped = &blockingStatusQueue{statusQueue: sq}
	}

	return sink, wrapped, nil
}

// ── StatusSink implementation ─────────────────────────────────────────────────

type maniflexStatusSink struct {
	srv  *maniflex.Server
	meta *maniflex.ModelMeta
}

func (s *maniflexStatusSink) Transition(
	ctx context.Context,
	id string,
	from, to jobs.Status,
	info jobs.StatusInfo,
) error {
	db := s.srv.DB()
	if db == nil || s.meta == nil {
		return fmt.Errorf("jobs/maniflex: StatusSink not initialised (DB not set)")
	}

	switch {
	case from == "" && to == jobs.StatusEnqueued:
		// Initial row created when a job enters the queue.
		return s.create(ctx, db, id, to, info)

	case from == jobs.StatusEnqueued && to == jobs.StatusRunning && info.Attempt == 1:
		// Row already exists from enqueue; update it to running.
		// Fall back to create for deployments that call the worker directly
		// without the wrapped queue (status row was never created at enqueue time).
		if err := s.update(ctx, db, id, to, info); err != nil {
			if errors.Is(err, maniflex.ErrNotFound) {
				return s.create(ctx, db, id, to, info)
			}
			return err
		}
		return nil

	default:
		return s.update(ctx, db, id, to, info)
	}
}

func (s *maniflexStatusSink) create(ctx context.Context, db maniflex.DBAdapter, id string, status jobs.Status, info jobs.StatusInfo) error {
	data := map[string]any{
		"id":        id,
		"type":      info.JobType,
		"status":    string(status),
		"actor_id":  info.ActorID,
		"tenant_id": info.TenantID,
		"attempts":  info.Attempt,
	}
	if status == jobs.StatusRunning {
		now := time.Now()
		data["started_at"] = now
	}
	if info.Error != "" {
		data["error"] = info.Error
	}
	rec, _ := maniflex.MapToRecord(s.meta, data)
	_, err := db.Create(ctx, s.meta, rec)
	return err
}

func (s *maniflexStatusSink) update(ctx context.Context, db maniflex.DBAdapter, id string, status jobs.Status, info jobs.StatusInfo) error {
	data := map[string]any{
		"status":   string(status),
		"attempts": info.Attempt,
	}
	if info.Error != "" {
		data["error"] = info.Error
	}
	// Set started_at on the first transition to running (retry attempts keep the original).
	if status == jobs.StatusRunning && info.Attempt == 1 {
		now := time.Now()
		data["started_at"] = now
	}
	if status == jobs.StatusSucceeded || status == jobs.StatusDead ||
		status == jobs.StatusFailed || status == jobs.StatusCancelled {
		now := time.Now()
		data["completed_at"] = now
	}
	if info.Result != nil {
		data["result_url"] = info.Result.URL
		data["result_mime"] = info.Result.Mime
	}
	rec, _ := maniflex.MapToRecord(s.meta, data)
	present := make(map[string]struct{}, len(data))
	for k := range data {
		present[k] = struct{}{}
	}
	_, err := db.Update(ctx, s.meta, id, rec, present)
	return err
}

// ── statusQueue ───────────────────────────────────────────────────────────────
// statusQueue wraps a jobs.Queue and creates a status row for every enqueued job.
// It also proxies Source and Cancellable so callers can pass a single value to
// both the enqueue site and jobs.WorkerConfig.Source.

type statusQueue struct {
	inner jobs.Queue
	sink  *maniflexStatusSink
}

// recordEnqueued creates the initial "enqueued" status row, ignoring errors so
// a sink misconfiguration never blocks the enqueue path.
func (q *statusQueue) recordEnqueued(ctx context.Context, id string, j jobs.Job) {
	_ = q.sink.Transition(ctx, id, "", jobs.StatusEnqueued, jobs.StatusInfo{
		JobType:  j.Type,
		ActorID:  j.ActorID,
		TenantID: j.TenantID,
	})
}

func (q *statusQueue) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	id, err := q.inner.Enqueue(ctx, j)
	if err != nil {
		return "", err
	}
	q.recordEnqueued(ctx, id, j)
	return id, nil
}

func (q *statusQueue) EnqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	id, err := q.inner.EnqueueAt(ctx, j, at)
	if err != nil {
		return "", err
	}
	q.recordEnqueued(ctx, id, j)
	return id, nil
}

func (q *statusQueue) EnqueueBatch(ctx context.Context, js []jobs.Job) ([]string, error) {
	ids, err := q.inner.EnqueueBatch(ctx, js)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if i < len(js) {
			q.recordEnqueued(ctx, id, js[i])
		}
	}
	return ids, nil
}

func (q *statusQueue) Close() error { return q.inner.Close() }

// ── Source (required by Worker) ───────────────────────────────────────────────

func (q *statusQueue) src() (jobs.Source, error) {
	s, ok := q.inner.(jobs.Source)
	if !ok {
		return nil, fmt.Errorf("jobs: inner queue does not implement jobs.Source")
	}
	return s, nil
}

func (q *statusQueue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	s, err := q.src()
	if err != nil {
		return nil, err
	}
	return s.Dequeue(ctx, n)
}

func (q *statusQueue) Ack(ctx context.Context, id string) error {
	s, err := q.src()
	if err != nil {
		return err
	}
	return s.Ack(ctx, id)
}

func (q *statusQueue) Nack(ctx context.Context, id string, jobErr error, delay time.Duration) error {
	s, err := q.src()
	if err != nil {
		return err
	}
	return s.Nack(ctx, id, jobErr, delay)
}

func (q *statusQueue) Dead(ctx context.Context, id string, jobErr error) error {
	s, err := q.src()
	if err != nil {
		return err
	}
	return s.Dead(ctx, id, jobErr)
}

// Cancel implements jobs.Cancellable. Delegates to the inner queue (which must
// implement Cancellable) and then updates the status row to "cancelled".
func (q *statusQueue) Cancel(ctx context.Context, id string) error {
	c, ok := q.inner.(jobs.Cancellable)
	if !ok {
		return fmt.Errorf("jobs: inner queue does not implement jobs.Cancellable")
	}
	if err := c.Cancel(ctx, id); err != nil {
		return err
	}
	// Update status row; ignore sink errors so the cancel itself is not undone.
	_ = q.sink.Transition(ctx, id, "", jobs.StatusCancelled, jobs.StatusInfo{})
	return nil
}

// ── blockingStatusQueue ───────────────────────────────────────────────────────
// Returned by Mount when the inner queue implements jobs.BlockingSource,
// so the worker can advertise the interface and avoid idle polling.

type blockingStatusQueue struct {
	*statusQueue
}

func (q *blockingStatusQueue) DequeueBlocking(ctx context.Context, n int, max time.Duration) ([]jobs.Job, error) {
	return q.inner.(jobs.BlockingSource).DequeueBlocking(ctx, n, max)
}

// ── compile-time interface checks ─────────────────────────────────────────────

var (
	_ jobs.Queue          = (*statusQueue)(nil)
	_ jobs.Source         = (*statusQueue)(nil)
	_ jobs.Cancellable    = (*statusQueue)(nil)
	_ jobs.BlockingSource = (*blockingStatusQueue)(nil)
)
