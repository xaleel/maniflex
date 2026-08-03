package sqlcore

// increment.go — maniflex.Incrementer for both the adapter and the transaction
// (audit WR-2).
//
// The statement is built in one place and used by both, for the reason the
// restore statement is: a primitive whose whole value is atomicity must not
// behave differently depending on whether a transaction happens to be open, and
// a hand-copied second builder is how that difference arrives.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
)

// Increment implements maniflex.Incrementer on the pooled connection.
func (a *Adapter) Increment(ctx context.Context, model *maniflex.ModelMeta, id string,
	deltas map[string]any, bounds map[string]maniflex.NumericBounds, qp *maniflex.QueryParams,
) (any, error) {
	return runIncrement(ctx, a.writeDb, a.driver, a.errNormalizer,
		incrementReq{model: model, id: id, deltas: deltas, bounds: bounds, qp: qp},
		func(ctx context.Context) (any, error) {
			return a.FindByID(ctx, model, id, &maniflex.QueryParams{})
		})
}

// Increment implements maniflex.Incrementer on the transaction connection, so an
// increment inside a transaction is rolled back with it.
func (t *txAdapter) Increment(ctx context.Context, model *maniflex.ModelMeta, id string,
	deltas map[string]any, bounds map[string]maniflex.NumericBounds, qp *maniflex.QueryParams,
) (any, error) {
	return runIncrement(ctx, t.tx, t.driver, t.errNormalizer,
		incrementReq{model: model, id: id, deltas: deltas, bounds: bounds, qp: qp},
		func(ctx context.Context) (any, error) {
			return t.FindByID(ctx, model, id, &maniflex.QueryParams{})
		})
}

// incrementReq is one increment's inputs, kept together so the shared runner and
// the statement builder take the same thing.
type incrementReq struct {
	model  *maniflex.ModelMeta
	id     string
	deltas map[string]any
	bounds map[string]maniflex.NumericBounds
	qp     *maniflex.QueryParams
}

// execer is the subset of *sql.DB and *sql.Tx the increment needs.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// runIncrement issues the UPDATE and turns its result into the record, or into
// the right error.
//
// The distinction the follow-up read exists for: the UPDATE matches nothing both
// when the row is absent (or out of scope) and when a bound would have been
// crossed, and those mean opposite things to a caller — one is permanent, the
// other may succeed on the next attempt once stock is back. The extra query runs
// only on the failure path.
func runIncrement(ctx context.Context, db execer, driver maniflex.DriverType,
	norm ErrorNormalizer, r incrementReq, reread func(context.Context) (any, error),
) (any, error) {
	query, args := incrementStmt(r.model, r.id, r.deltas, r.bounds, r.qp, driver)
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("increment: %w", normalizeErr(norm, err, r.model.TableName))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		exists, err := rowExists(ctx, db, r.model, r.id, r.qp, driver)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, maniflex.ErrNotFound
		}
		return nil, maniflex.ErrIncrementOutOfBounds
	}
	return reread(ctx)
}

// rowExists reports whether the target row is present and in scope, ignoring the
// bound guard. Used only to explain a zero-row increment.
func rowExists(ctx context.Context, db execer, model *maniflex.ModelMeta, id string,
	qp *maniflex.QueryParams, driver maniflex.DriverType,
) (bool, error) {
	qr, ok := db.(interface {
		QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	})
	if !ok {
		return false, errors.New("sqlcore: increment: connection cannot query")
	}
	p := &ph{driver: driver}
	conds := incrementScopeConds(model, id, qp, driver, p)
	var one int
	err := qr.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE %s LIMIT 1",
		q(model.TableName), strings.Join(conds, " AND ")), p.args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("increment: %w", err)
	}
	return true, nil
}

// incrementScopeConds renders the conditions that identify the target row: its
// id, the soft-delete marker, and the request's forced scope. They are shared by
// the UPDATE and the existence probe so the probe cannot answer "present" for a
// row the UPDATE would never have matched.
func incrementScopeConds(model *maniflex.ModelMeta, id string, qp *maniflex.QueryParams,
	driver maniflex.DriverType, p *ph,
) []string {
	conds := []string{fmt.Sprintf("%s = %s", q("id"), p.add(id))}
	if cond := softDeleteCond(model, model.TableName, driver); cond != "" {
		conds = append(conds, cond)
	}
	if scope := restoreScopeCond(model, qp, driver, p); scope != "" {
		conds = append(conds, scope)
	}
	return conds
}

// incrementStmt builds the UPDATE that applies deltas under its bounds.
//
// The guard is expressed on the post-increment value — `"stock" + ? >= 0`, not
// `"stock" >= 3` — so it says exactly what it means regardless of the delta's
// sign, and a positive increment against a min bound is correctly a no-op rather
// than a spurious refusal.
//
// Columns are emitted in sorted order so the statement is deterministic: the
// same call produces the same SQL, which keeps prepared-statement caches warm
// and makes the tests able to assert on it.
func incrementStmt(model *maniflex.ModelMeta, id string, deltas map[string]any,
	bounds map[string]maniflex.NumericBounds, qp *maniflex.QueryParams,
	driver maniflex.DriverType,
) (string, []any) {
	cols := make([]string, 0, len(deltas))
	for col := range deltas {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	// Placeholders are added in the order they appear in the finished statement,
	// and that ordering is load-bearing rather than tidiness: on SQLite a "?" is
	// bound positionally by where it sits in the SQL text, so a placeholder
	// registered out of order silently takes a different argument. Building the
	// guards during the SET loop — their natural place — put the bound's value
	// where updated_at's belonged and scrambled every argument after it.
	// Postgres would have been fine, since $N binds by number; the SQLite lane is
	// what catches this, so the sections below must stay in text order.
	p := &ph{driver: driver}

	// 1. SET, the deltas.
	sets := make([]string, 0, len(cols)+1)
	for _, col := range cols {
		sets = append(sets, fmt.Sprintf("%s = %s + %s", q(col), q(col), p.add(deltas[col])))
	}
	// 2. SET, updated_at.
	if f := model.FieldByDBName("updated_at"); f != nil {
		sets = append(sets, fmt.Sprintf("%s = %s", q("updated_at"), p.add(normalise(time.Now().UTC()))))
	}
	// 3. WHERE, the row's identity and scope.
	conds := incrementScopeConds(model, id, qp, driver, p)
	// 4. WHERE, the bound guards.
	for _, col := range cols {
		b, ok := bounds[col]
		if !ok {
			continue
		}
		// The delta is bound again here rather than reusing the SET's
		// placeholder, for the same positional reason: on SQLite a repeated "?"
		// consumes the NEXT argument, not the same one.
		if b.Min != nil {
			conds = append(conds, fmt.Sprintf("%s + %s >= %s",
				q(col), p.add(deltas[col]), p.add(*b.Min)))
		}
		if b.Max != nil {
			conds = append(conds, fmt.Sprintf("%s + %s <= %s",
				q(col), p.add(deltas[col]), p.add(*b.Max)))
		}
	}

	return fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		q(model.TableName), strings.Join(sets, ", "), strings.Join(conds, " AND ")), p.args
}
