package maniflex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// historyInsertMaxRetries bounds the retry loop in appendHistoryRow. Each retry
// recomputes nextVersion + reinserts after a UNIQUE collision on the (record_id,
// version) index — the bound exists so a runaway insert storm (caller hammering
// the same record from many goroutines) can't loop indefinitely. Sized to
// absorb realistic write fan-in (tens of concurrent edits to the same record);
// production callers that exceed this should pair versioning with a write
// queue or explicit per-record serialisation.
const historyInsertMaxRetries = 30

// ── History meta synthesis ────────────────────────────────────────────────────

// synthesizeHistoryMeta builds a read-only ModelMeta for the {model}_history
// sibling table. It has no Go struct — fields are constructed directly.
func synthesizeHistoryMeta(source *ModelMeta) *ModelMeta {
	histTable := toSnakeCase(source.Name) + "_history"
	histName := source.Name + "History"

	strType := reflect.TypeOf("")
	intType := reflect.TypeOf(int(0))
	timeType := reflect.TypeOf(time.Time{})
	pstrType := reflect.TypeOf((*string)(nil))

	makeField := func(goName, dbName, jsonName string, goType reflect.Type, filterable, sortable bool) FieldMeta {
		return FieldMeta{
			Name: goName,
			Type: goType,
			Tags: FieldTags{
				DBName:     dbName,
				JSONName:   jsonName,
				Filterable: filterable,
				Sortable:   sortable,
			},
		}
	}

	fields := []FieldMeta{
		makeField("ID", "id", "id", strType, false, false),
		makeField("RecordID", "record_id", "record_id", strType, true, false),
		makeField("Version", "version", "version", intType, false, true),
		makeField("Operation", "operation", "operation", strType, true, false),
		makeField("ActorID", "actor_id", "actor_id", pstrType, true, false),
		makeField("Timestamp", "timestamp", "timestamp", timeType, false, true),
		makeField("RequestID", "request_id", "request_id", strType, false, false),
		makeField("Diff", "diff", "diff", strType, false, false),
	}
	if !source.Config.VersionedDiffOnly {
		fields = append(fields, makeField("Snapshot", "snapshot", "snapshot", pstrType, false, false))
	}

	meta := &ModelMeta{
		Name:      histName,
		TableName: histTable,
		Fields:    fields,
		// Headless (audit MS-4): the history model is registered and migrated
		// like any other, but mounts no REST routes of its own. It used to mount
		// the full read surface at /{model}_history, and because per-model
		// middleware is registered ForModel(parent) — not ForModel(parentHistory)
		// — an app that protected Invoice with ModelConfig.Middleware.Auth left
		// GET /invoice_history unauthenticated and unscoped: every tenant's
		// history, readable by anyone. History is now reached only through
		// GET /:model/{id}/history, which runs the parent's read pipeline first.
		Config: ModelConfig{Headless: true},
		Indices: []IndexSpec{{
			// UNIQUE protects against the (record_id, version) duplicate that
			// could happen when two concurrent writes to the same record both
			// computed the same nextVersion. appendHistoryRow retries on the
			// constraint violation, so callers see a well-formed history row
			// rather than a silent duplicate.
			Name:    "uidx_" + histTable + "_record_version",
			Columns: []string{"record_id", "version DESC"},
			Unique:  true,
		}},
	}
	return meta
}

// ── Versioning middleware registration ───────────────────────────────────────

// registerVersioningFor registers the DB middleware pair for a versioned model
// and adds the history meta to the registry.
func (c *Server) registerVersioningFor(meta *ModelMeta) error {
	histMeta := synthesizeHistoryMeta(meta)
	// History model is read-only — register it so it gets migrated and routed.
	if err := c.registry.add(histMeta); err != nil {
		// Already registered (e.g. called twice) — not fatal.
		return nil
	}

	modelName := meta.Name
	steps := c.steps // capture pointer; adapter is resolved lazily at call time

	// ── Block writes to the history table ────────────────────────────────────
	blockWrite := func(ctx *ServerContext, next func() error) error {
		ctx.Abort(405, "METHOD_NOT_ALLOWED", "history table is read-only")
		return nil
	}
	for _, op := range []Operation{OpCreate, OpUpdate, OpDelete} {
		c.Pipeline.DB.Register(blockWrite,
			ForModel(histMeta.Name),
			ForOperation(op),
			AtPosition(Before),
			WithName("versioning-block-write-"+string(op)),
		)
	}

	// ── Before (update + delete): capture pre-image ───────────────────────────
	capturePreImage := func(ctx *ServerContext, next func() error) error {
		if ctx.ResourceID == "" {
			return next()
		}
		exec := dbExec{adapter: ctx.Model.ResolveAdapter(steps.adapter), tx: ctx.Tx}
		pre, err := exec.FindByID(ctx.Ctx, ctx.Model, ctx.ResourceID,
			&QueryParams{Limit: 1, Page: 1})
		if err == nil {
			ctx.Set("history.pre", pre)
		}
		return next()
	}
	c.Pipeline.DB.Register(capturePreImage,
		ForModel(modelName),
		ForOperation(OpUpdate),
		AtPosition(Before),
		WithName("versioning-capture-pre"),
	)
	c.Pipeline.DB.Register(capturePreImage,
		ForModel(modelName),
		ForOperation(OpDelete),
		AtPosition(Before),
		WithName("versioning-capture-pre-delete"),
	)

	// ── After (create/update/delete): write history row ───────────────────────
	writeHistory := func(ctx *ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		// Skip if the primary operation failed.
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}
		exec := dbExec{adapter: ctx.Model.ResolveAdapter(steps.adapter), tx: ctx.Tx}
		if err := appendHistoryRow(ctx, exec, meta, histMeta); err != nil {
			// History write failure should not break the primary response.
			ctx.Logger().Error("versioning: history write failed",
				"model", modelName, "error", err)
		}
		return nil
	}
	for _, op := range []Operation{OpCreate, OpUpdate, OpDelete} {
		c.Pipeline.DB.Register(writeHistory,
			ForModel(modelName),
			ForOperation(op),
			AtPosition(After),
			WithName("versioning-write-"+string(op)),
		)
	}
	return nil
}

// ── History read (GET /:model/{id}/history) ──────────────────────────────────

// defaultHistoryPageSize bounds a history page when the request names no limit.
// History is monotonic and a long-lived record accumulates rows without bound,
// so an unpaginated read is a slow query waiting to happen.
const defaultHistoryPageSize = 20

// readHistory serves OpReadHistory: the version history of one record.
//
// Order matters here, and it is the security property (audit MS-4). The parent
// record is read **first**, through the same scoped query a GET of that record
// would use, so a caller who may not see the record cannot see its history
// either — they get the same 404 the record itself would give them, which is
// also what stops the endpoint becoming an existence oracle. Only then are the
// history rows fetched, filtered to that record id.
//
// The alternative — filtering the history table by tenant — is not available at
// any price: the table holds id, record_id, version, operation, actor_id,
// timestamp, request_id, diff and snapshot, and none of those is the parent's
// tenant or owner column. Scoping has to borrow the parent's.
func (s *defaultSteps) readHistory(ctx *ServerContext, exec dbExec, model *ModelMeta) error {
	histMeta, ok := s.reg.Get(model.Name + "History")
	if !ok {
		// Versioned is set but registerVersioningFor never ran — a wiring bug,
		// not a client error.
		ctx.Abort(http.StatusInternalServerError, "HISTORY_UNAVAILABLE",
			"history model is not registered for "+model.Name)
		return nil
	}

	// Gate on the parent, carrying only the scope the server imposed. The
	// request's own filters and sorts are meant for the history list below;
	// applying them to the parent would 404 on any filter naming a column the
	// parent does not share.
	var scope []*FilterExpr
	if ctx.Query != nil {
		scope = forcedFilters(ctx.Query.Filters)
	}
	if err := gateOnParent(ctx, exec, model, scope); err != nil {
		return err // ErrNotFound → 404, exactly as a read of the record would
	}

	page, limit := 1, defaultHistoryPageSize
	if ctx.Query != nil {
		if ctx.Query.Page > 0 {
			page = ctx.Query.Page
		}
		if ctx.Query.Limit > 0 {
			limit = ctx.Query.Limit
		}
	}

	rows, total, err := exec.FindMany(ctx.Ctx, histMeta, &QueryParams{
		Page:  page,
		Limit: limit,
		// Forced: the caller asked for this record's history and must not be
		// able to widen it to another record's by sending a filter of their own.
		Filters: []*FilterExpr{{
			Field: "record_id", Operator: OpEq, Value: ctx.ResourceID,
			Group: -1, Forced: true,
		}},
		// Newest first — the question a history endpoint is usually asked.
		Sorts: []SortExpr{{DBName: "version", Direction: SortDesc}},
	})
	if err != nil {
		return err
	}

	items := make([]any, len(rows))
	for i, row := range rows {
		items[i] = row
	}
	ctx.DBResult = &ListResult{Items: items, Total: total, Query: ctx.Query}
	return nil
}

// gateOnParent decides whether the caller may see this record's history, by
// asking whether they may see the record. Returns ErrNotFound when they may not
// — the same answer the record itself would give, so the endpoint cannot be used
// to learn that an id exists in someone else's tenant.
//
// A soft-deleted record still has history worth reading, and the delete entry is
// usually the one being looked for, so the gate prefers the adapter's
// ScopeChecker, which counts soft-deleted rows as present while applying the
// scope in full. An adapter that does not implement it falls back to a normal
// scoped read; the only difference is that a soft-deleted record's history 404s.
//
// A hard-deleted record's history is unreachable either way. There is no row left
// to authorise against, and answering from the history table alone would mean
// choosing between leaking it to everyone and denying it to everyone.
func gateOnParent(ctx *ServerContext, exec dbExec, model *ModelMeta, scope []*FilterExpr) error {
	if sc, ok := exec.scopeChecker(); ok {
		found, err := sc.ExistsInScope(ctx.Ctx, model, ctx.ResourceID, scope)
		if err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return nil
	}
	_, err := exec.FindByID(ctx.Ctx, model, ctx.ResourceID, &QueryParams{
		Page: 1, Limit: 1, Filters: scope,
	})
	return err
}

// ── History row writer ────────────────────────────────────────────────────────

func appendHistoryRow(ctx *ServerContext, exec dbExec, source, histMeta *ModelMeta) error {
	operation := string(ctx.Operation)

	// Get pre-image (update/delete) and post-state (create/update).
	var pre, post map[string]any
	if raw, ok := ctx.Get("history.pre"); ok {
		pre, _ = raw.(map[string]any)
	}
	switch ctx.Operation {
	case OpCreate, OpUpdate:
		if m, ok := ctx.DBResult.(map[string]any); ok {
			post = m
		}
	}

	diff := computeDiff(source, pre, post, ctx.Operation)
	diffJSON, _ := json.Marshal(diff)

	var snapshotPtr *string
	if !source.Config.VersionedDiffOnly {
		snap := redactSnapshot(source, chooseSnapshot(pre, post, ctx.Operation))
		snapJSON, _ := json.Marshal(snap)
		s := string(snapJSON)
		snapshotPtr = &s
	}

	// Determine record_id: from post on create, from ResourceID otherwise.
	recordID := ctx.ResourceID
	if ctx.Operation == OpCreate && post != nil {
		if id, ok := post["id"].(string); ok && id != "" {
			recordID = id
		}
	}
	if recordID == "" {
		return nil // nothing to track
	}

	var actorID *string
	if ctx.Auth != nil && ctx.Auth.UserID != "" {
		s := ctx.Auth.UserID
		actorID = &s
	}

	row := map[string]any{
		"id":         uuid.New().String(),
		"record_id":  recordID,
		"operation":  operation,
		"actor_id":   actorID,
		"timestamp":  time.Now().UTC(),
		"request_id": ctx.RequestID,
		"diff":       string(diffJSON),
	}
	if !source.Config.VersionedDiffOnly && snapshotPtr != nil {
		row["snapshot"] = *snapshotPtr
	}

	// nextVersion isn't transactionally serialised — two writes targeting the
	// same record can compute the same version. The UNIQUE (record_id, version)
	// index on the history table catches that as an *ErrConstraint, and we
	// recompute + retry. A fresh row id per attempt keeps the insert idempotent.
	var lastErr error
	for attempt := 0; attempt < historyInsertMaxRetries; attempt++ {
		version, err := nextVersion(ctx, exec, histMeta, recordID)
		if err != nil {
			return fmt.Errorf("get next version: %w", err)
		}
		row["id"] = uuid.New().String()
		row["version"] = version

		if _, err := exec.Create(ctx.Ctx, histMeta, row); err == nil {
			return nil
		} else {
			var ce *ErrConstraint
			if !errors.As(err, &ce) {
				return err
			}
			lastErr = err
			// Briefly yield so concurrent writers can commit and the next
			// nextVersion read sees a fresh snapshot. Without this, all
			// retries on contended SQLite WAL paths can observe the same
			// stale count.
			time.Sleep(time.Duration(attempt+1) * time.Millisecond)
		}
	}
	return fmt.Errorf("history insert: %d retries exhausted on UNIQUE collision: %w",
		historyInsertMaxRetries, lastErr)
}

func nextVersion(ctx *ServerContext, exec dbExec, histMeta *ModelMeta, recordID string) (int, error) {
	_, total, err := exec.FindMany(ctx.Ctx, histMeta, &QueryParams{
		Filters: []*FilterExpr{{
			Field: "record_id", Operator: OpEq, Value: recordID, Group: -1,
		}},
		Page: 1, Limit: 1,
	})
	if err != nil {
		return 0, err
	}
	return int(total) + 1, nil
}

// computeDiff builds the diff map for a history row.
// Excluded: hidden, writeonly, encrypted, HMAC fields.
func computeDiff(model *ModelMeta, pre, post map[string]any, op Operation) map[string]map[string]any {
	diff := make(map[string]map[string]any)
	excluded := excludedDBNames(model)

	switch op {
	case OpCreate:
		for _, f := range model.Fields {
			if excluded[f.Tags.DBName] || f.Tags.DBName == "id" {
				continue
			}
			newVal := post[f.Tags.DBName]
			if newVal != nil {
				diff[f.Tags.JSONName] = map[string]any{"old": nil, "new": newVal}
			}
		}
	case OpUpdate:
		for _, f := range model.Fields {
			if excluded[f.Tags.DBName] || f.Tags.DBName == "id" {
				continue
			}
			oldVal := pre[f.Tags.DBName]
			newVal := post[f.Tags.DBName]
			if !equalValues(oldVal, newVal) {
				diff[f.Tags.JSONName] = map[string]any{"old": oldVal, "new": newVal}
			}
		}
	case OpDelete:
		for _, f := range model.Fields {
			if excluded[f.Tags.DBName] || f.Tags.DBName == "id" {
				continue
			}
			oldVal := pre[f.Tags.DBName]
			diff[f.Tags.JSONName] = map[string]any{"old": oldVal, "new": nil}
		}
	}
	return diff
}

// chooseSnapshot returns the full row state to store as the snapshot.
// For delete, the snapshot is the pre-image. For create/update it is the post-state.
func chooseSnapshot(pre, post map[string]any, op Operation) map[string]any {
	if op == OpDelete {
		return pre
	}
	return post
}

// redactSnapshot drops the columns that must never be recorded in history from
// a snapshot row, returning a copy so the caller's map (the live DBResult or
// pre-image) is left alone.
//
// The snapshot is built from an already-decrypted row, so without this the
// history table stores the plaintext of every encrypted column — and of hidden
// and write-only fields such as password hashes — defeating the at-rest
// guarantee for a table nobody thinks of as holding secrets. computeDiff has
// always applied this exclusion set; the snapshot must apply the same one, or
// the two halves of a history row disagree about what is a secret.
func redactSnapshot(model *ModelMeta, snap map[string]any) map[string]any {
	if snap == nil {
		return nil
	}
	excluded := excludedDBNames(model)
	out := make(map[string]any, len(snap))
	for k, v := range snap {
		if !excluded[k] {
			out[k] = v
		}
	}
	return out
}

// excludedDBNames returns the set of DB column names that must not appear in
// history records: hidden, writeonly, encrypted, and HMAC companion columns.
func excludedDBNames(model *ModelMeta) map[string]bool {
	out := make(map[string]bool)
	for _, f := range model.Fields {
		if f.Tags.Hidden || f.Tags.WriteOnly || f.Tags.Encrypted {
			out[f.Tags.DBName] = true
		}
	}
	// HMAC columns follow the pattern {field}_hmac
	for _, f := range model.Fields {
		if f.Tags.Encrypted {
			out[f.Tags.DBName+"_hmac"] = true
		}
	}
	return out
}

// equalValues compares two values from a DB row map for equality.
func equalValues(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

