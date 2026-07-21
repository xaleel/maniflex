package sqlcore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/xaleel/maniflex"

	"github.com/google/uuid"
)

// BeginTx starts a database transaction and returns a maniflex.Tx that routes all
// CRUD operations through the same *sql.Tx connection.
func (a *Adapter) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	tx, err := a.writeDb.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	return &txAdapter{
		tx:            tx,
		driver:        a.driver,
		reg:           a.reg,
		errNormalizer: a.errNormalizer,
	}, nil
}

// txAdapter wraps a *sql.Tx and implements maniflex.Tx using quoted identifiers
// throughout, matching the safety guarantees of Adapter.
type txAdapter struct {
	tx            *sql.Tx
	driver        maniflex.DriverType
	reg           maniflex.RegistryAccessor
	errNormalizer ErrorNormalizer
}

func (t *txAdapter) Commit() error   { return t.tx.Commit() }
func (t *txAdapter) Rollback() error { return t.tx.Rollback() }

// ExecContext exposes the underlying *sql.Tx for packages that need to run
// arbitrary SQL within the active transaction (e.g. events/outbox INSERT).
// Satisfies the events.SQLExecer interface.
func (t *txAdapter) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// RawQueryContext satisfies the maniflex.rawableT interface, routing ServerContext.RawQuery
// through the active transaction.
func (t *txAdapter) RawQueryContext(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := t.tx.QueryContext(ctx, rebind(t.driver, query), args...)
	if err != nil {
		return nil, fmt.Errorf("tx raw query: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// RawExecContext satisfies the maniflex.rawableT interface, routing ServerContext.RawExec
// through the active transaction.
func (t *txAdapter) RawExecContext(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, rebind(t.driver, query), args...)
	if err != nil {
		return 0, fmt.Errorf("tx raw exec: %w", err)
	}
	n, err := res.RowsAffected()
	return n, err
}

func (t *txAdapter) newPH() *ph { return &ph{driver: t.driver} }

// ── FindByID ─────────────────────────────────────────────────────────────────

// scanStruct mirrors Adapter.scanStruct for the transaction connection, building
// the column plan fresh (no per-tx cache).
func (t *txAdapter) scanStruct(rows *sql.Rows, model *maniflex.ModelMeta) ([]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	return scanStructRows(rows, model, t.driver, cols, buildColumnPlan(model, cols))
}

func (t *txAdapter) FindByID(ctx context.Context, model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams) (any, error) {
	if model.GoType == nil {
		m, err := t.findByIDMap(ctx, model, id, qp)
		if err != nil {
			return nil, err
		}
		return echoRecord(model, m)
	}
	p := t.newPH()
	joinSQL := buildJoins(model, qp.Filters, qp.Sorts)
	var conditions []string
	if cond := softDeleteCond(model, model.TableName, t.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))
	if len(qp.Filters) > 0 {
		if extra := filterConds(model, qp.Filters, t.driver, p); extra != "" {
			conditions = append(conditions, extra)
		}
	}
	query := fmt.Sprintf(
		"SELECT %s.* FROM %s%s WHERE %s LIMIT 1",
		q(model.TableName), q(model.TableName), joinSQL,
		strings.Join(conditions, " AND "),
	)
	rows, err := t.tx.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("tx FindByID: %w", err)
	}
	recs, err := t.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, maniflex.ErrNotFound
	}
	if err := populateIncludesTyped(ctx, t.tx, t.reg, t.driver, model, recs, qp); err != nil {
		return nil, err
	}
	return recs[0], nil
}

func (t *txAdapter) findByIDMap(ctx context.Context, model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams) (map[string]any, error) {
	p := t.newPH()

	joinSQL := buildJoins(model, qp.Filters, qp.Sorts)

	var conditions []string
	if cond := softDeleteCond(model, model.TableName, t.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))
	if len(qp.Filters) > 0 {
		if extra := filterConds(model, qp.Filters, t.driver, p); extra != "" {
			conditions = append(conditions, extra)
		}
	}

	query := fmt.Sprintf(
		"SELECT %s.* FROM %s%s WHERE %s LIMIT 1",
		q(model.TableName), q(model.TableName), joinSQL,
		strings.Join(conditions, " AND "),
	)

	rows, err := t.tx.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("tx FindByID: %w", err)
	}
	defer rows.Close()

	results, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, maniflex.ErrNotFound
	}

	result := results[0]
	if err := populateIncludes(ctx, t.tx, t.reg, t.driver, model, []map[string]any{result}, qp); err != nil {
		return nil, err
	}
	return result, nil
}

// ── FindByIDForUpdate ─────────────────────────────────────────────────────────

// FindByIDForUpdate fetches the row and acquires a pessimistic write lock.
// Postgres appends FOR UPDATE; SQLite does a plain SELECT (the lock is at the
// transaction level, taken at BEGIN — db/sqlite opens write connections with
// _txlock=immediate).
func (t *txAdapter) FindByIDForUpdate(ctx context.Context, model *maniflex.ModelMeta, id string) (any, error) {
	p := t.newPH()

	var conditions []string
	if cond := softDeleteCond(model, model.TableName, t.driver); cond != "" {
		conditions = append(conditions, cond)
	}
	conditions = append(conditions, fmt.Sprintf("%s.%s = %s", q(model.TableName), q("id"), p.add(id)))

	query := fmt.Sprintf(
		"SELECT %s.* FROM %s WHERE %s LIMIT 1",
		q(model.TableName), q(model.TableName),
		strings.Join(conditions, " AND "),
	)
	if t.driver == maniflex.Postgres {
		query += " FOR UPDATE"
	}

	rows, err := t.tx.QueryContext(ctx, query, p.args...)
	if err != nil {
		return nil, fmt.Errorf("tx FindByIDForUpdate: %w", err)
	}
	if model.GoType == nil {
		results, err := scanRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, maniflex.ErrNotFound
		}
		return maniflex.MapToRecord(model, results[0])
	}
	recs, err := t.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, maniflex.ErrNotFound
	}
	return recs[0], nil
}

// ── FindMany ─────────────────────────────────────────────────────────────────

func (t *txAdapter) FindMany(ctx context.Context, model *maniflex.ModelMeta, qp *maniflex.QueryParams) ([]any, int64, error) {
	if model.GoType == nil {
		results, total, err := t.findManyMap(ctx, model, qp)
		if err != nil {
			return nil, 0, err
		}
		recs := make([]any, len(results))
		for i, m := range results {
			rec, _ := echoRecord(model, m)
			recs[i] = rec
		}
		return recs, total, nil
	}

	joinSQL := buildJoins(model, qp.Filters, qp.Sorts) + ftsJoinSQL(model, qp, t.driver)

	total := int64(-1)
	if qp.Cursor == nil {
		cp := t.newPH()
		countConds := allWhereConds(model, qp.Filters, t.driver, cp)
		countConds = appendSearchCond(countConds, model, qp, t.driver, cp)
		countWhere := condToSQL(countConds)
		countQuery := countQuerySQL(model.TableName, joinSQL, countWhere)
		if err := t.tx.QueryRowContext(ctx, countQuery, cp.args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("tx count: %w", err)
		}
	}

	dp := t.newPH()
	dataConds := allWhereConds(model, qp.Filters, t.driver, dp)
	dataConds = appendSearchCond(dataConds, model, qp, t.driver, dp)
	var orderSQL, limitSQL string
	if qp.Cursor != nil {
		dataConds, orderSQL, limitSQL = cursorDataClauses(model, qp.Cursor, qp.Limit, dataConds, dp)
	} else {
		orderSQL = listOrderSQL(model, qp, t.driver, dp)
		limitSQL = fmt.Sprintf(" LIMIT %s OFFSET %s", dp.add(qp.Limit), dp.add(qp.Offset()))
	}
	dataWhere := condToSQL(dataConds)
	dataQuery := fmt.Sprintf(
		"SELECT %s.* FROM %s%s%s%s%s",
		q(model.TableName), q(model.TableName), joinSQL, dataWhere, orderSQL, limitSQL,
	)
	rows, err := t.tx.QueryContext(ctx, dataQuery, dp.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("tx FindMany: %w", err)
	}
	recs, err := t.scanStruct(rows, model)
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	if qp.Cursor != nil {
		recs = recs[:finalizeCursorPage(qp.Cursor, len(recs), qp.Limit, func(i int) (any, string) {
			m := maniflex.RecordToMap(model, recs[i])
			return m[qp.Cursor.Field], fmt.Sprint(m["id"])
		})]
	}
	if err := populateIncludesTyped(ctx, t.tx, t.reg, t.driver, model, recs, qp); err != nil {
		return nil, 0, err
	}
	return recs, total, nil
}

func (t *txAdapter) findManyMap(ctx context.Context, model *maniflex.ModelMeta, qp *maniflex.QueryParams) ([]map[string]any, int64, error) {
	joinSQL := buildJoins(model, qp.Filters, qp.Sorts) + ftsJoinSQL(model, qp, t.driver)

	// ─ count ─ (skipped in cursor mode — keyset pagination reports has_more.)
	total := int64(-1)
	if qp.Cursor == nil {
		cp := t.newPH()
		countConds := allWhereConds(model, qp.Filters, t.driver, cp)
		countConds = appendSearchCond(countConds, model, qp, t.driver, cp)
		countWhere := condToSQL(countConds)
		countQuery := countQuerySQL(model.TableName, joinSQL, countWhere)
		if err := t.tx.QueryRowContext(ctx, countQuery, cp.args...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("tx count: %w", err)
		}
	}

	// ─ data ─
	dp := t.newPH()
	dataConds := allWhereConds(model, qp.Filters, t.driver, dp)
	dataConds = appendSearchCond(dataConds, model, qp, t.driver, dp)
	var orderSQL, limitSQL string
	if qp.Cursor != nil {
		dataConds, orderSQL, limitSQL = cursorDataClauses(model, qp.Cursor, qp.Limit, dataConds, dp)
	} else {
		orderSQL = listOrderSQL(model, qp, t.driver, dp)
		limitSQL = fmt.Sprintf(" LIMIT %s OFFSET %s", dp.add(qp.Limit), dp.add(qp.Offset()))
	}
	dataWhere := condToSQL(dataConds)

	dataQuery := fmt.Sprintf(
		"SELECT %s.* FROM %s%s%s%s%s",
		q(model.TableName), q(model.TableName), joinSQL, dataWhere, orderSQL, limitSQL,
	)
	rows, err := t.tx.QueryContext(ctx, dataQuery, dp.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("tx FindMany: %w", err)
	}
	defer rows.Close()

	results, err := scanRows(rows)
	if err != nil {
		return nil, 0, err
	}

	if qp.Cursor != nil {
		results = results[:finalizeCursorPage(qp.Cursor, len(results), qp.Limit, func(i int) (any, string) {
			return results[i][qp.Cursor.Field], fmt.Sprint(results[i]["id"])
		})]
	}

	if err := populateIncludes(ctx, t.tx, t.reg, t.driver, model, results, qp); err != nil {
		return nil, 0, err
	}

	return results, total, nil
}

// ── Create ────────────────────────────────────────────────────────────────────

func (t *txAdapter) Create(ctx context.Context, model *maniflex.ModelMeta, record any) (any, error) {
	// Typed write path (W4); see Adapter.Create. Encryption/synthetic records with
	// extra columns fall back to the map path.
	if ptr, ok := structForWrite(model, record); ok {
		query, args := buildInsertSQL(model, ptr, maniflex.PresentColumns(record), t.driver, normaliseTx)
		m, err := t.execInsert(ctx, model, query, args, recordID(model, ptr))
		if err != nil {
			return nil, err
		}
		return echoRecord(model, m)
	}
	data := recordData(model, record)
	m, err := t.createMap(ctx, model, data)
	if err != nil {
		return nil, err
	}
	return echoRecord(model, m)
}

// execInsert runs a prebuilt INSERT on the tx connection, mirroring
// Adapter.execInsert. Shared by the typed and map (createMap) write paths.
func (t *txAdapter) execInsert(ctx context.Context, model *maniflex.ModelMeta, query string, args []any, id string) (map[string]any, error) {
	if t.driver == maniflex.Postgres {
		rows, err := t.tx.QueryContext(ctx, query+" RETURNING *", args...)
		if err != nil {
			return nil, fmt.Errorf("tx create: %w", normalizeErr(t.errNormalizer, err, model.TableName))
		}
		defer rows.Close()
		results, err := scanRows(rows)
		if err != nil || len(results) == 0 {
			return nil, err
		}
		return results[0], nil
	}
	if _, err := t.tx.ExecContext(ctx, query, args...); err != nil {
		return nil, fmt.Errorf("tx create: %w", normalizeErr(t.errNormalizer, err, model.TableName))
	}
	return t.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
}

func (t *txAdapter) createMap(ctx context.Context, model *maniflex.ModelMeta, data map[string]any) (map[string]any, error) {
	if s, _ := data["id"].(string); s == "" {
		data["id"] = uuid.New().String()
	}
	injectTimestamps(model, data, true)

	p := t.newPH()
	cols := make([]string, 0, len(data))
	phs := make([]string, 0, len(data))

	for col, val := range data {
		cols = append(cols, q(col))
		phs = append(phs, p.add(normaliseTx(val)))
	}

	query := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		q(model.TableName),
		strings.Join(cols, ", "),
		strings.Join(phs, ", "),
	)
	return t.execInsert(ctx, model, query, p.args, fmt.Sprint(data["id"]))
}

// ── Update ────────────────────────────────────────────────────────────────────

func (t *txAdapter) Update(ctx context.Context, model *maniflex.ModelMeta, id string, record any, present map[string]struct{}) (any, error) {
	// Typed write path (W4); see Adapter.Update.
	if ptr, ok := structForWrite(model, record); ok {
		if len(present) == 0 {
			m, err := t.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
			if err != nil {
				return nil, err
			}
			return echoRecord(model, m)
		}
		query, args := buildUpdateSQL(model, id, ptr, present, t.driver, normaliseTx)
		m, err := t.execUpdate(ctx, model, query, args, id)
		if err != nil {
			return nil, err
		}
		return echoRecord(model, m)
	}
	full := recordData(model, record)
	data := make(map[string]any, len(present))
	for col := range present {
		if v, ok := full[col]; ok {
			data[col] = v
		}
	}
	m, err := t.updateMap(ctx, model, id, data)
	if err != nil {
		return nil, err
	}
	return echoRecord(model, m)
}

func (t *txAdapter) updateMap(ctx context.Context, model *maniflex.ModelMeta, id string, data map[string]any) (map[string]any, error) {
	if len(data) == 0 {
		return t.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
	}
	injectTimestamps(model, data, false)

	p := t.newPH()
	sets := make([]string, 0, len(data))
	for col, val := range data {
		sets = append(sets, fmt.Sprintf("%s = %s", q(col), p.add(normaliseTx(val))))
	}

	// Soft-delete-scope the WHERE so a deleted row matches zero rows → ErrNotFound,
	// matching Adapter.updateMap and the typed buildUpdate path above.
	where := fmt.Sprintf("%s = %s", q("id"), p.add(id))
	if c := softDeleteCond(model, model.TableName, t.driver); c != "" {
		where += " AND " + c
	}

	query := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s",
		q(model.TableName),
		strings.Join(sets, ", "),
		where,
	)
	return t.execUpdate(ctx, model, query, p.args, id)
}

// execUpdate runs a prebuilt UPDATE on the tx connection, mirroring
// Adapter.execUpdate. Shared by the typed and map (updateMap) write paths.
func (t *txAdapter) execUpdate(ctx context.Context, model *maniflex.ModelMeta, query string, args []any, id string) (map[string]any, error) {
	if t.driver == maniflex.Postgres {
		rows, err := t.tx.QueryContext(ctx, query+" RETURNING *", args...)
		if err != nil {
			return nil, fmt.Errorf("tx update: %w", normalizeErr(t.errNormalizer, err, model.TableName))
		}
		defer rows.Close()
		results, err := scanRows(rows)
		if err != nil {
			return nil, err
		}
		if len(results) == 0 {
			return nil, maniflex.ErrNotFound
		}
		return results[0], nil
	}
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tx update: %w", normalizeErr(t.errNormalizer, err, model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, maniflex.ErrNotFound
	}
	return t.findByIDMap(ctx, model, id, &maniflex.QueryParams{Limit: 1, Page: 1})
}

// ── Delete ────────────────────────────────────────────────────────────────────

func (t *txAdapter) Delete(ctx context.Context, model *maniflex.ModelMeta, id string) error {
	if model.SoftDelete.Enabled {
		return t.softDelete(ctx, model, id)
	}
	p := t.newPH()
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", q(model.TableName), q("id"), p.add(id))
	res, err := t.tx.ExecContext(ctx, query, p.args...)
	if err != nil {
		// Normalize so a database-enforced onDelete:restrict FK violation becomes a
		// clean 409 (5.16), the way create/update already normalize their constraint
		// errors — otherwise it surfaces as an opaque 500.
		return fmt.Errorf("tx delete: %w", normalizeErr(t.errNormalizer, err, model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return maniflex.ErrNotFound
	}
	return nil
}

// HardDelete physically removes the row identified by id, bypassing soft-delete
// even on a soft-deletable model. The scheduled-transition runner (8.6) uses it
// for mfx:"scheduled;hard-delete" fields. Returns ErrNotFound when absent.
func (t *txAdapter) HardDelete(ctx context.Context, model *maniflex.ModelMeta, id string) error {
	p := t.newPH()
	query := fmt.Sprintf("DELETE FROM %s WHERE %s = %s", q(model.TableName), q("id"), p.add(id))
	res, err := t.tx.ExecContext(ctx, query, p.args...)
	if err != nil {
		return fmt.Errorf("tx hard delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return maniflex.ErrNotFound
	}
	return nil
}

// softDelete mirrors (*Adapter).softDelete, including its "not already deleted"
// guard: without it an UPDATE against an already-deleted row still reports one
// affected row, so re-deleting it re-stamps the column and reports success
// instead of ErrNotFound.
func (t *txAdapter) softDelete(ctx context.Context, model *maniflex.ModelMeta, id string) error {
	p := t.newPH()
	col := model.SoftDelete.Field
	var setExpr string
	var whereNotDelExpr string
	if model.SoftDelete.FieldType == maniflex.SoftDeleteBool {
		setExpr = fmt.Sprintf("%s = %s", q(col), p.add(true))
		whereNotDelExpr = fmt.Sprintf("NOT %s", q(col))
	} else {
		setExpr = fmt.Sprintf("%s = %s", q(col), p.add(time.Now().UTC()))
		whereNotDelExpr = fmt.Sprintf("%s IS NULL", q(col))
	}
	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s AND %s",
		q(model.TableName), setExpr, q("id"), p.add(id), whereNotDelExpr)
	res, err := t.tx.ExecContext(ctx, query, p.args...)
	if err != nil {
		return fmt.Errorf("tx soft delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return maniflex.ErrNotFound
	}
	return nil
}

// Restore implements maniflex.Restorer for the transaction connection (5.19),
// sharing restoreStmt with (*Adapter).Restore so the two cannot drift — the
// hazard softDelete/HardDelete have to guard against by hand.
//
// Being in the transaction matters: the restore and whatever else the request
// does commit or roll back together, so a restore whose response fails to build
// does not leave the row half-resurrected.
func (t *txAdapter) Restore(ctx context.Context, model *maniflex.ModelMeta, id string,
	qp *maniflex.QueryParams,
) (any, error) {
	query, args, err := restoreStmt(model, id, qp, t.driver)
	if err != nil {
		return nil, err
	}
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tx restore: %w", normalizeErr(t.errNormalizer, err, model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, maniflex.ErrNotFound
	}
	// The scope was enforced by the UPDATE, so the read-back needs no filters of
	// its own — but it does need a non-nil QueryParams, which FindByID
	// dereferences.
	return t.FindByID(ctx, model, id, &maniflex.QueryParams{})
}

// ExistsInScope implements maniflex.ScopeChecker on the transaction, sharing the
// statement with the adapter so the two cannot drift apart on what "in scope"
// means. Inside a transaction it also sees the request's own uncommitted writes.
func (t *txAdapter) ExistsInScope(ctx context.Context, model *maniflex.ModelMeta, id string,
	filters []*maniflex.FilterExpr,
) (bool, error) {
	query, args := existsInScopeStmt(model, id, filters, t.driver)
	var one int
	err := t.tx.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tx ExistsInScope query: %w", err)
	}
	return true, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func normaliseTx(v any) any {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		v = rv.Elem().Interface()
	}
	if t, ok := v.(time.Time); ok {
		return maniflex.CanonicalTime(t)
	}
	if ls, ok := v.(maniflex.LocaleString); ok {
		if b, err := json.Marshal(ls); err == nil {
			return string(b)
		}
	}
	return v
}

// Ensure *txAdapter implements maniflex.Tx at compile time.
var _ maniflex.Tx = (*txAdapter)(nil)
