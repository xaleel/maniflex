// Package sql provides a *sql.DB-backed job queue for maniflex.
//
// It supports both PostgreSQL and SQLite and provides transactional outbox
// semantics: when Enqueue is called with an active Server transaction in ctx
// (set by maniflex.WithTransaction), the INSERT runs through the same *sql.Tx so
// the job row commits or rolls back together with the surrounding business
// write.
//
// Outbox wiring is enabled by default via jobs/sql/maniflex_glue.go (build tag
// !nomaniflex_glue). Binaries that do not use maniflex can exclude the glue file
// with -tags nomaniflex_glue.
package sql

import (
	stdsql "database/sql"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/jobs"
)

const defaultLeaseDuration = 5 * time.Minute

// sqlExecer is satisfied by *sql.Tx (and by sqlcore.txAdapter via its
// ExecContext method). The maniflex_glue.go init() sets txFromContext to extract
// one from a maniflex.Tx stored in context.
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (stdsql.Result, error)
}

// txFromContext is set by maniflex_glue.go when the nomaniflex_glue build tag is absent.
// It extracts an sqlExecer from a maniflex.Tx stored in context, or returns nil
// when there is no active Server transaction.
var txFromContext func(context.Context) sqlExecer

const defaultTableName = "job_queue"

// PayloadCipher encrypts and decrypts the job payload column at rest. Provide one
// via WithPayloadCipher when jobs may carry sensitive data; without it payloads
// are stored as cleartext JSON.
type PayloadCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// Option configures a Queue (and, via the same options, Migrate).
type Option func(*config)

type config struct {
	table  string
	cipher PayloadCipher
	driver string // "" = auto-detect; "postgres" or "sqlite" to force
	lease  time.Duration
}

func newConfig(opts []Option) config {
	c := config{table: defaultTableName}
	for _, o := range opts {
		o(&c)
	}
	if c.table == "" {
		c.table = defaultTableName
	}
	if c.lease <= 0 {
		c.lease = defaultLeaseDuration
	}
	return c
}

// WithTableName runs the queue on a table other than the default "job_queue",
// so two independent queues (e.g. an isolated OTP lane) can share one database.
// Pass the same option to both New and Migrate. Index names are derived from the
// table name so they don't collide.
func WithTableName(name string) Option { return func(c *config) { c.table = name } }

// WithPayloadCipher encrypts the payload column at rest with the given cipher.
// The stored value is prefixed "encq:" so encrypted and legacy cleartext rows can
// coexist. Pass the same cipher to New wherever the queue is read or written.
func WithPayloadCipher(c PayloadCipher) Option { return func(cfg *config) { cfg.cipher = c } }

// WithDriver forces the SQL dialect instead of auto-detecting it from the
// driver. Pass "postgres" or "sqlite" — the same value you pass to Migrate.
//
// New otherwise guesses from db.Driver()'s package path, which recognises
// lib/pq and jackc/pgx. Set this when you use a Postgres driver it does not
// recognise, or simply to be explicit: the dialect decides both the SQL
// (Postgres uses FOR UPDATE SKIP LOCKED) and the placeholder style ($1 vs ?),
// so a wrong guess does not run slower — it fails outright (audit JB-6).
func WithDriver(name string) Option { return func(c *config) { c.driver = name } }

// WithLeaseDuration sets the visibility timeout: how long a claimed job stays
// invisible to other workers before another Dequeue may reclaim it (its
// lease_until is stamped now+d on claim). Default 5m.
//
// It must exceed how long a handler runs, or a still-running job is reclaimed and
// executed a second time. A long-running handler does not need a large value if
// the worker renews the lease — the Worker does this automatically for a Source
// implementing jobs.LeaseRenewer — since renewal extends the lease and never
// shortens it (audit JB-10). A non-positive value keeps the default.
func WithLeaseDuration(d time.Duration) Option { return func(c *config) { c.lease = d } }

// Queue is both a jobs.Queue (producer) and a jobs.Source (consumer).
// It also implements jobs.Cancellable, jobs.Inspector, and jobs.LeaseRenewer.
type Queue struct {
	db     *stdsql.DB
	isPG   bool // true = Postgres, false = SQLite
	table  string
	cipher PayloadCipher
	lease  time.Duration // visibility timeout stamped on claim
}

// New creates a Queue backed by db. The dialect is auto-detected from
// db.Driver() unless WithDriver forces it.
func New(db *stdsql.DB, opts ...Option) *Queue {
	c := newConfig(opts)
	return &Queue{db: db, isPG: resolveIsPG(c.driver, db), table: c.table, cipher: c.cipher, lease: c.lease}
}

// resolveIsPG decides the dialect: an explicit WithDriver value wins, otherwise
// the driver is inspected. An unrecognised explicit value falls through to
// detection rather than silently forcing a dialect.
func resolveIsPG(explicit string, db *stdsql.DB) bool {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "postgres", "postgresql", "pgx":
		return true
	case "sqlite", "sqlite3":
		return false
	default:
		return detectPostgres(db)
	}
}

// q rewrites the default quoted "job_queue" table reference to the configured
// table. A no-op when the default table is used.
func (q *Queue) q(query string) string {
	if q.table == defaultTableName {
		return query
	}
	return strings.ReplaceAll(query, `"`+defaultTableName+`"`, `"`+q.table+`"`)
}

func detectPostgres(db *stdsql.DB) bool {
	pkgPath, name := driverIdent(reflect.TypeOf(db.Driver()))
	return isPostgresDriver(pkgPath, name)
}

// driverIdent returns the import path and short type name of a driver type,
// unwrapping the pointer most drivers register (e.g. *stdlib.Driver). A pointer
// type has no PkgPath of its own, so the element must be taken first.
func driverIdent(t reflect.Type) (pkgPath, name string) {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil {
		return "", ""
	}
	return t.PkgPath(), t.String()
}

// isPostgresDriver classifies a driver as Postgres from its package path, which
// is stable across versions, with the old short-name heuristic as a fallback.
// The original check matched only "pq"/"postgres", so jackc/pgx — whose driver
// is package "stdlib", type "stdlib.Driver" — was misread as SQLite and the
// whole adapter spoke the wrong dialect (audit JB-6).
func isPostgresDriver(pkgPath, name string) bool {
	for _, m := range []string{"jackc/pgx", "lib/pq", "cockroachdb"} {
		if strings.Contains(pkgPath, m) {
			return true
		}
	}
	// Fallback for drivers not matched by path: the pre-existing heuristic.
	lower := strings.ToLower(name)
	return strings.Contains(lower, "pq") || strings.Contains(lower, "postgres")
}

// ── Queue (producer) ──────────────────────────────────────────────────────────

func (q *Queue) Enqueue(ctx context.Context, j jobs.Job) (string, error) {
	return q.enqueueAt(ctx, j, time.Now())
}

func (q *Queue) EnqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	return q.enqueueAt(ctx, j, at)
}

func (q *Queue) EnqueueBatch(ctx context.Context, js []jobs.Job) ([]string, error) {
	ids := make([]string, len(js))
	for i, j := range js {
		id, err := q.enqueueAt(ctx, j, time.Now())
		if err != nil {
			return ids, err
		}
		ids[i] = id
	}
	return ids, nil
}

func (q *Queue) Close() error { return q.db.Close() }

func (q *Queue) enqueueAt(ctx context.Context, j jobs.Job, at time.Time) (string, error) {
	if j.ID == "" {
		j.ID = newID()
	}
	if j.MaxRetry == 0 {
		j.MaxRetry = 3
	}
	payload, err := q.marshalPayload(j.Payload)
	if err != nil {
		return "", err
	}
	headers, err := marshalHeaders(j.Headers)
	if err != nil {
		return "", err
	}
	now := ts(time.Now())
	nb := ts(at)

	p := q.newPH()
	query := fmt.Sprintf(
		`INSERT INTO "job_queue" ("id","type","payload","status","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts","created_at","updated_at") VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
		p.Add(j.ID), p.Add(j.Type), p.Add(payload),
		p.Add(string(jobs.StatusEnqueued)),
		p.Add(j.TraceID), p.Add(j.ActorID), p.Add(j.TenantID),
		p.Add(j.MaxRetry), p.Add(j.Priority), p.Add(nb),
		p.Add(j.GroupKey), p.Add(headers), p.Add(0),
		p.Add(now), p.Add(now),
	)

	// Outbox path: run through the active Server transaction when available.
	if txFromContext != nil {
		if exec := txFromContext(ctx); exec != nil {
			_, err = exec.ExecContext(ctx, q.q(query), p.Args()...)
			return j.ID, err
		}
	}

	_, err = q.db.ExecContext(ctx, q.q(query), p.Args()...)
	return j.ID, err
}

// ── Source (consumer) ─────────────────────────────────────────────────────────

// claimedColumns is what Dequeue reads back from the rows it claimed. Shared by
// both drivers so the two claim statements cannot drift apart.
const claimedColumns = `"id","type","payload","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts"`

// maxClaimRetries bounds the Postgres group-collision retry loop (see Dequeue).
// A collision resolves as soon as the winning transaction commits and its key
// becomes visible as running, so one retry almost always suffices; the bound
// only guards against a pathological run of overlapping claims.
const maxClaimRetries = 3

// Dequeue claims up to n ready jobs.
//
// Both drivers claim and return in a single statement, so the rows a caller
// receives are exactly the rows it stamped (audit JB-1). Both also dedupe by
// group_key within the claim, so a single call never takes two jobs sharing a
// non-empty GroupKey — the WHERE clause alone cannot prevent that, because its
// "group not already running" subquery is evaluated against the pre-UPDATE
// state where none of this batch's jobs are running yet (audit JB-2).
//
// Postgres additionally needs FOR UPDATE SKIP LOCKED to keep concurrent
// claimers off each other's candidates. That still leaves a cross-transaction
// race: two Dequeues snapshot before either commits, so neither sees the
// other's claim, and at the LIMIT boundary they can pick different jobs of the
// same key. The job_queue_group_running partial unique index makes that state
// impossible — the losing UPDATE fails — and this loop retries it, by which
// point the winner's key is visible as running and is excluded. SQLite's
// database-level write lock serialises whole claims, so it never reaches here.
func (q *Queue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	if n <= 0 {
		return nil, nil
	}
	for attempt := 0; ; attempt++ {
		now := time.Now()
		leaseUntil := ts(now.Add(q.lease))
		nowStr := ts(now)

		var (
			claimed []jobs.Job
			err     error
		)
		if q.isPG {
			claimed, err = q.dequeuePG(ctx, n, nowStr, leaseUntil)
		} else {
			claimed, err = q.dequeueSQLite(ctx, n, nowStr, leaseUntil)
		}
		if err == nil {
			return claimed, nil
		}
		if q.isGroupRunningViolation(err) && attempt < maxClaimRetries {
			continue
		}
		return nil, err
	}
}

// rankedDedup wraps src so it yields every empty-key job plus the single
// highest-priority job of each non-empty key — the row_number()=1 of its
// partition. A claim built from it therefore takes at most one job per key,
// which the WHERE clause alone cannot guarantee (audit JB-2). src is the
// relation to rank over: the FOR-UPDATE-locked CTE on Postgres, the base-table
// ready set on SQLite. It must expose "id", "group_key", "priority" and
// "created_at"; the wrapper re-exposes the last three so callers can order and
// limit the survivors.
func rankedDedup(src string) string {
	return `SELECT "id","priority","created_at" FROM (
        SELECT "id","group_key","priority","created_at",
            ROW_NUMBER() OVER (
                PARTITION BY "group_key" ORDER BY "priority" DESC, "created_at" ASC
            ) AS "rn"
        FROM ` + src + `
    ) "ranked"
    WHERE "group_key" = '' OR "rn" = 1`
}

func (q *Queue) dequeuePG(ctx context.Context, n int, nowStr, leaseUntil string) ([]jobs.Job, error) {
	p := q.newPH()
	// The dedup runs over the locked set, not the base table: Postgres forbids
	// FOR UPDATE alongside a window function in one query level, so the CTE
	// locks candidates first (SKIP LOCKED for throughput) and ROW_NUMBER ranks
	// what it locked. LIMIT is on the locked set, so a burst of one key can
	// crowd the batch — acceptable, since that key runs one-at-a-time regardless.
	// Placeholders are added in the order they appear in the string.
	query := fmt.Sprintf(`
WITH "locked" AS (
    SELECT "id","group_key","priority","created_at" FROM "job_queue"
    WHERE "status" IN ('enqueued','failed')
      AND "not_before" <= %s
      AND ("lease_until" IS NULL OR "lease_until" < %s)
      AND ("group_key" = '' OR "group_key" NOT IN (
          SELECT DISTINCT "group_key" FROM "job_queue"
          WHERE "status" = 'running' AND "group_key" != ''
      ))
    ORDER BY "priority" DESC, "created_at" ASC
    LIMIT %s
    FOR UPDATE SKIP LOCKED
)
UPDATE "job_queue"
SET "status" = 'running', "lease_until" = %s, "attempts" = "attempts" + 1, "updated_at" = %s
WHERE "id" IN (SELECT "id" FROM (`+rankedDedup(`"locked"`)+`) "deduped")
RETURNING `+claimedColumns,
		p.Add(nowStr), p.Add(nowStr), p.Add(n),
		p.Add(leaseUntil), p.Add(nowStr),
	)
	rows, err := q.db.QueryContext(ctx, q.q(query), p.Args()...)
	if err != nil {
		return nil, fmt.Errorf("jobs/sql: dequeue: %w", err)
	}
	defer rows.Close()
	return q.scanJobs(rows)
}

// dequeueSQLite claims and returns in one statement.
//
// It used to be two: an UPDATE that stamped lease_until, then a SELECT that
// re-found "the rows we just claimed" with WHERE lease_until = <that string>.
// SQLite's write lock serialises the UPDATE but does not span the pair, and
// lease_until is derived from time.Now() — so any two claims whose clock reads
// agree share a lease string, and each one's SELECT matches the other's rows.
// That is not exotic: Windows' system clock granularity is coarse, and
// RFC3339Nano drops trailing zeros, so back-to-back calls routinely render the
// same string. It produced three failures at once (audit JB-1):
//
//   - the same job returned to several workers, so it ran several times;
//   - the same job returned by successive calls on a single worker, with no
//     concurrency involved at all;
//   - rows stamped running with attempts incremented that no caller ever
//     received, which then sat until the lease expired having burned a retry
//     without executing — and eventually dead-lettered without ever running.
//
// RETURNING removes the identification step entirely: the statement hands back
// precisely the rows it updated, so there is nothing to match on and no window
// to interleave in. Requires SQLite 3.35 (2021). The dedup subquery adds the
// group_key guarantee (audit JB-2); the write lock covers concurrency, so no
// retry is needed here.
func (q *Queue) dequeueSQLite(ctx context.Context, n int, nowStr, leaseUntil string) ([]jobs.Job, error) {
	p := q.newPH()
	// SQLite uses positional "?" placeholders, so every p.Add below is called in
	// the exact left-to-right order its placeholder appears in the query: the two
	// SET values first, then the two inside the ready subquery, then LIMIT. The
	// ready set is ranked in place (no FOR UPDATE — SQLite has none, and its
	// write lock already serialises the whole statement), then the survivors are
	// ordered and limited.
	ready := `(
        SELECT "id","group_key","priority","created_at" FROM "job_queue"
        WHERE "status" IN ('enqueued','failed')
          AND "not_before" <= %s
          AND ("lease_until" IS NULL OR "lease_until" < %s)
          AND ("group_key" = '' OR "group_key" NOT IN (
              SELECT DISTINCT "group_key" FROM "job_queue"
              WHERE "status" = 'running' AND "group_key" != ''
          ))
    )`
	query := fmt.Sprintf(`
UPDATE "job_queue"
SET "status" = 'running', "lease_until" = %s, "attempts" = "attempts" + 1, "updated_at" = %s
WHERE "id" IN (
    SELECT "id" FROM (`+rankedDedup(ready)+`) "deduped"
    ORDER BY "priority" DESC, "created_at" ASC
    LIMIT %s
)
RETURNING `+claimedColumns,
		p.Add(leaseUntil), p.Add(nowStr), // SET
		p.Add(nowStr), p.Add(nowStr), // ready: not_before, lease_until
		p.Add(n), // LIMIT
	)
	rows, err := q.db.QueryContext(ctx, q.q(query), p.Args()...)
	if err != nil {
		return nil, fmt.Errorf("jobs/sql: dequeue: %w", err)
	}
	defer rows.Close()
	return q.scanJobs(rows)
}

// isGroupRunningViolation reports whether err is the job_queue_group_running
// unique-index violation raised when a concurrent claim would put a second job
// of one key into running. It matches on the index name rather than a driver
// error code, so it holds for both pq and pgx (both name the index in the
// message) without importing either.
func (q *Queue) isGroupRunningViolation(err error) bool {
	if err == nil {
		return false
	}
	idx := q.table + "_group_running"
	msg := err.Error()
	return strings.Contains(msg, idx) &&
		(strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate"))
}

func (q *Queue) Ack(ctx context.Context, id string) error {
	p := q.newPH()
	now := ts(time.Now())
	_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "status"='succeeded',"completed_at"=%s,"updated_at"=%s WHERE "id"=%s`,
		p.Add(now), p.Add(now), p.Add(id),
	)), p.Args()...)
	return err
}

func (q *Queue) Nack(ctx context.Context, id string, jobErr error, delay time.Duration) error {
	errMsg := ""
	if jobErr != nil {
		errMsg = jobErr.Error()
	}
	// Read current attempts to decide retry vs dead.
	var attempts, maxRetry int
	p := q.newPH()
	row := q.db.QueryRowContext(ctx,
		q.q(fmt.Sprintf(`SELECT "attempts","max_retry" FROM "job_queue" WHERE "id"=%s`, p.Add(id))),
		p.Args()...,
	)
	if err := row.Scan(&attempts, &maxRetry); err != nil {
		return fmt.Errorf("jobs/sql: nack scan: %w", err)
	}
	if maxRetry == 0 {
		maxRetry = 3
	}

	p = q.newPH()
	now := ts(time.Now())
	if attempts >= maxRetry {
		_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
			`UPDATE "job_queue" SET "status"='dead',"last_error"=%s,"updated_at"=%s WHERE "id"=%s`,
			p.Add(errMsg), p.Add(now), p.Add(id),
		)), p.Args()...)
		return err
	}
	nb := ts(time.Now().Add(delay))
	_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "status"='failed',"last_error"=%s,"not_before"=%s,"lease_until"=NULL,"updated_at"=%s WHERE "id"=%s`,
		p.Add(errMsg), p.Add(nb), p.Add(now), p.Add(id),
	)), p.Args()...)
	return err
}

// Requeue returns j to the queue without spending a retry attempt: it rewrites
// the row to enqueued with j's attempt count and headers (which the Worker uses
// to carry the unhandled-requeue counter) and clears the lease. Unlike Nack it
// does not consult or advance the retry budget — the Worker uses it for a job
// of a type this worker cannot handle, so another worker can claim it, and an
// unhandled round-trip must not erode the budget a real handler will need
// (audit JB-4/JB-9). The stored attempts is j.Attempts as given; the next claim
// re-increments it, so the effective count is unchanged across the round-trip.
func (q *Queue) Requeue(ctx context.Context, j jobs.Job, delay time.Duration) error {
	headers, err := marshalHeaders(j.Headers)
	if err != nil {
		return fmt.Errorf("jobs/sql: requeue headers: %w", err)
	}
	p := q.newPH()
	now := ts(time.Now())
	nb := ts(time.Now().Add(delay))
	_, err = q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "status"='enqueued',"attempts"=%s,"headers"=%s,"not_before"=%s,"lease_until"=NULL,"updated_at"=%s WHERE "id"=%s`,
		p.Add(j.Attempts), p.Add(headers), p.Add(nb), p.Add(now), p.Add(j.ID),
	)), p.Args()...)
	if err != nil {
		return fmt.Errorf("jobs/sql: requeue: %w", err)
	}
	return nil
}

func (q *Queue) Dead(ctx context.Context, id string, jobErr error) error {
	errMsg := ""
	if jobErr != nil {
		errMsg = jobErr.Error()
	}
	p := q.newPH()
	now := ts(time.Now())
	_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "status"='dead',"last_error"=%s,"updated_at"=%s WHERE "id"=%s`,
		p.Add(errMsg), p.Add(now), p.Add(id),
	)), p.Args()...)
	return err
}

// ── LeaseRenewer ──────────────────────────────────────────────────────────────

func (q *Queue) RenewLease(ctx context.Context, id string, d time.Duration) error {
	p := q.newPH()
	now := time.Now()
	until := ts(now.Add(d))
	nowStr := ts(now)

	// Never shorten. A renewal extends a lease; it must not reduce one. The worker
	// renews with a short horizon (LeaseRenew*3, 90s by default) that is smaller
	// than the initial visibility timeout, so a plain assignment would *cut* a
	// freshly claimed job's lease and let it be reclaimed far sooner than the
	// timeout promises (audit JB-10). Take the later of the current lease and
	// now+d. MAX/GREATEST compares the fixed-width timestamp text, which sorts
	// chronologically (audit JB-7); COALESCE guards the NULL case — not expected on
	// a running row, but a bare MAX(NULL, x) is NULL on SQLite, which would null the
	// lease out and make the running job immediately reclaimable.
	maxFn := "MAX"
	if q.isPG {
		maxFn = "GREATEST"
	}
	_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "lease_until"=%s(COALESCE("lease_until", %s), %s),"updated_at"=%s WHERE "id"=%s AND "status"='running'`,
		maxFn, p.Add(until), p.Add(until), p.Add(nowStr), p.Add(id),
	)), p.Args()...)
	return err
}

// ── Cancellable ───────────────────────────────────────────────────────────────

func (q *Queue) Cancel(ctx context.Context, id string) error {
	p := q.newPH()
	now := ts(time.Now())
	res, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "status"='cancelled',"updated_at"=%s WHERE "id"=%s AND "status" IN ('enqueued','failed')`,
		p.Add(now), p.Add(id),
	)), p.Args()...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("jobs/sql: job %s not found or already running/completed", id)
	}
	return nil
}

// ── Inspector ─────────────────────────────────────────────────────────────────

func (q *Queue) Get(ctx context.Context, id string) (jobs.JobState, error) {
	p := q.newPH()
	rows, err := q.db.QueryContext(ctx, q.q(fmt.Sprintf(
		`SELECT "id","type","payload","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts","status","last_error","created_at","updated_at","completed_at"
FROM "job_queue" WHERE "id"=%s LIMIT 1`, p.Add(id),
	)), p.Args()...)
	if err != nil {
		return jobs.JobState{}, err
	}
	defer rows.Close()
	states, err := q.scanJobStates(rows)
	if err != nil {
		return jobs.JobState{}, err
	}
	if len(states) == 0 {
		return jobs.JobState{}, fmt.Errorf("jobs/sql: job %s not found", id)
	}
	return states[0], nil
}

func (q *Queue) List(ctx context.Context, qry jobs.ListQuery) ([]jobs.JobState, error) {
	p := q.newPH()
	var conds []string
	if qry.Status != "" {
		conds = append(conds, fmt.Sprintf(`"status"=%s`, p.Add(string(qry.Status))))
	}
	if qry.Type != "" {
		conds = append(conds, fmt.Sprintf(`"type"=%s`, p.Add(qry.Type)))
	}
	if qry.ActorID != "" {
		conds = append(conds, fmt.Sprintf(`"actor_id"=%s`, p.Add(qry.ActorID)))
	}
	if qry.TenantID != "" {
		conds = append(conds, fmt.Sprintf(`"tenant_id"=%s`, p.Add(qry.TenantID)))
	}
	limit := qry.Limit
	if limit <= 0 {
		limit = 100
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	query := fmt.Sprintf(
		`SELECT "id","type","payload","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts","status","last_error","created_at","updated_at","completed_at"
FROM "job_queue"%s ORDER BY "created_at" DESC LIMIT %s OFFSET %s`,
		where, p.Add(limit), p.Add(qry.Offset),
	)
	rows, err := q.db.QueryContext(ctx, q.q(query), p.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return q.scanJobStates(rows)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (q *Queue) newPH() *sqlcore.PlaceholderBuilder {
	driver := maniflex.SQLite
	if q.isPG {
		driver = maniflex.Postgres
	}
	return sqlcore.NewPlaceholderBuilder(driver)
}

// tsLayout is RFC3339 with a fixed nine-digit fractional part.
//
// time.RFC3339Nano omits trailing zeros, so its strings vary in width: a
// whole-second stamp ("…56Z") and a fractional one ("…56.5Z") differ in the
// character right after the seconds — 'Z' (0x5A) versus '.' (0x2E). SQLite
// compares these columns as TEXT (lexicographically), so within one second the
// whole-second stamp sorts AFTER the fractional one, inverting time order: a due
// job can look not-due and a future job can look due, each by up to a second
// (audit JB-7). A fixed width removes the variability, so lexicographic order
// matches chronological order. Postgres stores TIMESTAMPTZ and compares
// chronologically regardless, and it parses this layout the same as before.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func ts(t time.Time) string {
	return t.UTC().Format(tsLayout)
}

func parseTS(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

const encPayloadPrefix = "encq:"

func (q *Queue) marshalPayload(p json.RawMessage) (string, error) {
	if len(p) == 0 {
		return "{}", nil
	}
	if q.cipher == nil {
		return string(p), nil
	}
	ciphertext, err := q.cipher.Encrypt([]byte(p))
	if err != nil {
		return "", fmt.Errorf("jobs/sql: encrypt payload: %w", err)
	}
	return encPayloadPrefix + hex.EncodeToString(ciphertext), nil
}

// unmarshalPayload reverses marshalPayload: decrypts an "encq:"-prefixed value
// when a cipher is configured, and passes cleartext (legacy) rows through.
func (q *Queue) unmarshalPayload(stored string) (json.RawMessage, error) {
	if q.cipher == nil || !strings.HasPrefix(stored, encPayloadPrefix) {
		return json.RawMessage(stored), nil
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(stored, encPayloadPrefix))
	if err != nil {
		return nil, fmt.Errorf("jobs/sql: decode payload: %w", err)
	}
	plaintext, err := q.cipher.Decrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("jobs/sql: decrypt payload: %w", err)
	}
	return json.RawMessage(plaintext), nil
}

func marshalHeaders(h map[string]string) (string, error) {
	if len(h) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(h)
	return string(b), err
}

func (q *Queue) scanJobs(rows *stdsql.Rows) ([]jobs.Job, error) {
	var out []jobs.Job
	for rows.Next() {
		var (
			id, typ, payload, traceID, actorID, tenantID string
			maxRetry, priority, attempts                 int
			notBefore, groupKey, headers                 string
		)
		if err := rows.Scan(&id, &typ, &payload, &traceID, &actorID, &tenantID,
			&maxRetry, &priority, &notBefore, &groupKey, &headers, &attempts); err != nil {
			return nil, err
		}
		decoded, err := q.unmarshalPayload(payload)
		if err != nil {
			return nil, err
		}
		j := jobs.Job{
			ID:        id,
			Type:      typ,
			Payload:   decoded,
			TraceID:   traceID,
			ActorID:   actorID,
			TenantID:  tenantID,
			MaxRetry:  maxRetry,
			Priority:  priority,
			GroupKey:  groupKey,
			Attempts:  attempts,
		}
		if nb := parseTS(notBefore); nb != nil {
			j.NotBefore = *nb
		}
		if headers != "" && headers != "{}" {
			_ = json.Unmarshal([]byte(headers), &j.Headers)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (q *Queue) scanJobStates(rows *stdsql.Rows) ([]jobs.JobState, error) {
	var out []jobs.JobState
	for rows.Next() {
		var (
			id, typ, payload, traceID, actorID, tenantID string
			maxRetry, priority, attempts                 int
			notBefore, groupKey, headers                 string
			status, lastErr                              string
			createdAt, updatedAt                         string
			completedAt                                  stdsql.NullString
		)
		if err := rows.Scan(
			&id, &typ, &payload, &traceID, &actorID, &tenantID,
			&maxRetry, &priority, &notBefore, &groupKey, &headers, &attempts,
			&status, &lastErr, &createdAt, &updatedAt, &completedAt,
		); err != nil {
			return nil, err
		}
		decoded, err := q.unmarshalPayload(payload)
		if err != nil {
			return nil, err
		}
		j := jobs.Job{
			ID:       id,
			Type:     typ,
			Payload:  decoded,
			TraceID:  traceID,
			ActorID:  actorID,
			TenantID: tenantID,
			MaxRetry: maxRetry,
			Priority: priority,
			GroupKey: groupKey,
			Attempts: attempts,
		}
		if headers != "" && headers != "{}" {
			_ = json.Unmarshal([]byte(headers), &j.Headers)
		}
		state := jobs.JobState{
			Job:    j,
			Status: jobs.Status(status),
			Error:  lastErr,
		}
		if t := parseTS(createdAt); t != nil {
			state.CreatedAt = *t
		}
		if t := parseTS(updatedAt); t != nil {
			state.UpdatedAt = *t
		}
		if completedAt.Valid {
			state.CompletedAt = parseTS(completedAt.String)
		}
		out = append(out, state)
	}
	return out, rows.Err()
}

// newID returns a time-prefixed random identifier using only stdlib.
// Format: 13 hex chars of millisecond timestamp + 19 hex chars of random = 32 chars total.
func newID() string {
	var rnd [10]byte
	_, _ = rand.Read(rnd[:])
	return fmt.Sprintf("%013x%s", time.Now().UnixMilli(), hex.EncodeToString(rnd[:]))
}

// Compile-time interface checks.
var (
	_ jobs.Queue        = (*Queue)(nil)
	_ jobs.Source       = (*Queue)(nil)
	_ jobs.Cancellable  = (*Queue)(nil)
	_ jobs.Inspector    = (*Queue)(nil)
	_ jobs.LeaseRenewer = (*Queue)(nil)
	_ jobs.Requeuer     = (*Queue)(nil)
)
