package sql

import (
	stdsql "database/sql"
	"context"
	"fmt"
	"strings"
)

// Migrate creates the job queue table if it does not exist.
// Call this once at startup before creating a Queue. driver must be "postgres"
// or "sqlite". Pass WithTableName to migrate a non-default table (the table and
// its indexes are renamed consistently so multiple queues can share a database).
func Migrate(ctx context.Context, db *stdsql.DB, driver string, opts ...Option) error {
	cfg := newConfig(opts)
	isPG := driver == "postgres"

	// rename rewrites the default table/index identifiers to the configured table.
	rename := func(s string) string {
		if cfg.table == defaultTableName {
			return s
		}
		return strings.ReplaceAll(s, defaultTableName, cfg.table)
	}

	tsType := "TEXT"
	if isPG {
		tsType = "TIMESTAMPTZ"
	}

	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "job_queue" (
  "id"           TEXT        NOT NULL PRIMARY KEY,
  "type"         TEXT        NOT NULL,
  "payload"      TEXT        NOT NULL DEFAULT '{}',
  "status"       TEXT        NOT NULL DEFAULT 'enqueued',
  "trace_id"     TEXT        NOT NULL DEFAULT '',
  "actor_id"     TEXT        NOT NULL DEFAULT '',
  "tenant_id"    TEXT        NOT NULL DEFAULT '',
  "max_retry"    INTEGER     NOT NULL DEFAULT 3,
  "priority"     INTEGER     NOT NULL DEFAULT 0,
  "not_before"   %[1]s       NOT NULL,
  "group_key"    TEXT        NOT NULL DEFAULT '',
  "headers"      TEXT        NOT NULL DEFAULT '{}',
  "attempts"     INTEGER     NOT NULL DEFAULT 0,
  "lease_until"  %[1]s       NULL,
  "last_error"   TEXT        NOT NULL DEFAULT '',
  "result"       TEXT        NULL,
  "created_at"   %[1]s       NOT NULL,
  "updated_at"   %[1]s       NOT NULL,
  "completed_at" %[1]s       NULL
)`, tsType)

	if _, err := db.ExecContext(ctx, rename(ddl)); err != nil {
		return fmt.Errorf("jobs/sql: migrate: %w", err)
	}

	// Indexes for common access patterns.
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS "job_queue_status_not_before" ON "job_queue" ("status","not_before")`,
		`CREATE INDEX IF NOT EXISTS "job_queue_type_status"       ON "job_queue" ("type","status")`,
		`CREATE INDEX IF NOT EXISTS "job_queue_tenant_status"     ON "job_queue" ("tenant_id","status")`,
		`CREATE INDEX IF NOT EXISTS "job_queue_actor_status"      ON "job_queue" ("actor_id","status")`,
		`CREATE INDEX IF NOT EXISTS "job_queue_group_key_status"  ON "job_queue" ("group_key","status")`,
	}
	for _, idx := range indexes {
		if _, err := db.ExecContext(ctx, rename(idx)); err != nil {
			return fmt.Errorf("jobs/sql: migrate index: %w", err)
		}
	}

	// The GroupKey invariant — at most one running job per non-empty key — is
	// enforced here, not only in the Dequeue query. The claim's own WHERE
	// clause is not enough on its own: on Postgres two concurrent Dequeue
	// transactions snapshot before either commits, so neither sees the other's
	// claim, and each can take a different job of the same key. This partial
	// unique index makes that state unrepresentable, so the losing transaction's
	// UPDATE fails rather than double-running the key; Dequeue catches it and
	// retries (audit JB-2).
	//
	// Both drivers support partial unique indexes. It covers only currently
	// running, non-empty-key rows — a small, transient set — but creating it
	// still fails if such a duplicate already exists from before this fix, which
	// is the correct loud signal: drain or clear the duplicate running jobs
	// first, then migrate.
	uniqIdx := `CREATE UNIQUE INDEX IF NOT EXISTS "job_queue_group_running" ON "job_queue" ("group_key") WHERE "status" = 'running' AND "group_key" != ''`
	if _, err := db.ExecContext(ctx, rename(uniqIdx)); err != nil {
		return fmt.Errorf("jobs/sql: migrate group-running index (a pre-existing "+
			"duplicate running job for one group_key blocks this — clear it first): %w", err)
	}
	return nil
}
