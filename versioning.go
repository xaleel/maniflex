package maniflex

import (
	"encoding/json"
	"errors"
	"fmt"
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
		Config:    ModelConfig{},
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

