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
}

func newConfig(opts []Option) config {
	c := config{table: defaultTableName}
	for _, o := range opts {
		o(&c)
	}
	if c.table == "" {
		c.table = defaultTableName
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

// Queue is both a jobs.Queue (producer) and a jobs.Source (consumer).
// It also implements jobs.Cancellable, jobs.Inspector, and jobs.LeaseRenewer.
type Queue struct {
	db     *stdsql.DB
	isPG   bool // true = Postgres, false = SQLite
	table  string
	cipher PayloadCipher
}

// New creates a Queue backed by db. The driver is auto-detected from db.Driver().
func New(db *stdsql.DB, opts ...Option) *Queue {
	c := newConfig(opts)
	return &Queue{db: db, isPG: detectPostgres(db), table: c.table, cipher: c.cipher}
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
	t := reflect.TypeOf(db.Driver()).String()
	return strings.Contains(t, "pq") || strings.Contains(t, "postgres")
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

// Dequeue claims up to n ready jobs. For Postgres it uses SELECT FOR UPDATE
// SKIP LOCKED; for SQLite it uses a subquery UPDATE with the implicit
// database-level write lock.
func (q *Queue) Dequeue(ctx context.Context, n int) ([]jobs.Job, error) {
	if n <= 0 {
		return nil, nil
	}
	now := time.Now()
	leaseUntil := ts(now.Add(defaultLeaseDuration))
	nowStr := ts(now)

	if q.isPG {
		return q.dequeuePG(ctx, n, nowStr, leaseUntil)
	}
	return q.dequeueSQLite(ctx, n, nowStr, leaseUntil)
}

func (q *Queue) dequeuePG(ctx context.Context, n int, nowStr, leaseUntil string) ([]jobs.Job, error) {
	p := q.newPH()
	query := fmt.Sprintf(`
WITH candidates AS (
    SELECT "id" FROM "job_queue"
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
WHERE "id" IN (SELECT "id" FROM candidates)
RETURNING "id","type","payload","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts"`,
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

func (q *Queue) dequeueSQLite(ctx context.Context, n int, nowStr, leaseUntil string) ([]jobs.Job, error) {
	p := q.newPH()
	// SQLite write lock serialises concurrent workers; no SKIP LOCKED needed.
	updateQ := fmt.Sprintf(`
UPDATE "job_queue"
SET "status" = 'running', "lease_until" = %s, "attempts" = "attempts" + 1, "updated_at" = %s
WHERE "id" IN (
    SELECT "id" FROM "job_queue"
    WHERE "status" IN ('enqueued','failed')
      AND "not_before" <= %s
      AND ("lease_until" IS NULL OR "lease_until" < %s)
      AND ("group_key" = '' OR "group_key" NOT IN (
          SELECT DISTINCT "group_key" FROM "job_queue"
          WHERE "status" = 'running' AND "group_key" != ''
      ))
    ORDER BY "priority" DESC, "created_at" ASC
    LIMIT %s
)`,
		p.Add(leaseUntil), p.Add(nowStr),
		p.Add(nowStr), p.Add(nowStr),
		p.Add(n),
	)
	if _, err := q.db.ExecContext(ctx, q.q(updateQ), p.Args()...); err != nil {
		return nil, fmt.Errorf("jobs/sql: dequeue update: %w", err)
	}

	// Fetch the rows we just claimed.
	p2 := q.newPH()
	selectQ := fmt.Sprintf(`
SELECT "id","type","payload","trace_id","actor_id","tenant_id","max_retry","priority","not_before","group_key","headers","attempts"
FROM "job_queue"
WHERE "status" = 'running' AND "lease_until" = %s
ORDER BY "priority" DESC, "created_at" ASC
LIMIT %s`,
		p2.Add(leaseUntil), p2.Add(n),
	)
	rows, err := q.db.QueryContext(ctx, q.q(selectQ), p2.Args()...)
	if err != nil {
		return nil, fmt.Errorf("jobs/sql: dequeue select: %w", err)
	}
	defer rows.Close()
	return q.scanJobs(rows)
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
	until := ts(time.Now().Add(d))
	now := ts(time.Now())
	_, err := q.db.ExecContext(ctx, q.q(fmt.Sprintf(
		`UPDATE "job_queue" SET "lease_until"=%s,"updated_at"=%s WHERE "id"=%s AND "status"='running'`,
		p.Add(until), p.Add(now), p.Add(id),
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

func ts(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
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
)
