package sqlcore

// write.go — typed struct→SQL write builders (typed-models migration,
// Phase 2 / T2.2). Productionizes the Phase-0 spike write builders, reusing the
// adapter's existing identifier quoting (q), placeholder builder (ph), and
// value normalisation (normalise: time→RFC3339, LocaleString/maps→JSON) so the
// generated SQL matches the map path's Create/Update exactly.
//
// Not yet wired into the public Adapter methods — that is Phase 3. Framework-only
// columns without a struct field (encryption *_hmac) are appended by the
// encryption hook in Phase 3, not here.

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/xaleel/maniflex"

	"github.com/google/uuid"
)

// buildInsert generates an INSERT for the struct pointed to by ptr (a *T for
// model.GoType). It generates id when empty (writing it back onto the struct so
// the caller can refetch it) and stamps created_at/updated_at, mirroring map
// Create + injectTimestamps. When present is nil every model column is written;
// when present is non-nil only those DB columns are written (plus id and the
// timestamp columns), matching createMap's PATCH-on-create column set exactly.
func (a *Adapter) buildInsert(model *maniflex.ModelMeta, ptr any, present map[string]struct{}) (string, []any) {
	return buildInsertSQL(model, ptr, present, a.driver, normalise)
}

// buildUpdate generates a PATCH-semantics UPDATE: only columns whose DB name is
// in present (plus updated_at) are written. present carries DB column names — the
// set the record carrier records (mapToRecord / SetField / bindRecord all key it
// by DBName). The WHERE is soft-delete-scoped so a deleted row matches zero rows
// → ErrNotFound, exactly like the map Update.
func (a *Adapter) buildUpdate(model *maniflex.ModelMeta, id string, ptr any, present map[string]struct{}) (string, []any) {
	return buildUpdateSQL(model, id, ptr, present, a.driver, normalise)
}

// buildInsertSQL / buildUpdateSQL are the driver- and normaliser-parameterised
// write builders shared by the pooled Adapter and the txAdapter (which passes
// normaliseTx). Keeping one implementation guarantees the transactional write
// path generates byte-identical SQL to the pooled one.
func buildInsertSQL(model *maniflex.ModelMeta, ptr any, present map[string]struct{}, driver maniflex.DriverType, norm func(any) any) (string, []any) {
	sv := reflect.ValueOf(ptr).Elem()
	now := time.Now().UTC()
	p := &ph{driver: driver}

	cols := make([]string, 0, len(model.Fields))
	phs := make([]string, 0, len(model.Fields))
	for i := range model.Fields {
		f := &model.Fields[i]
		col := f.Tags.DBName
		val := sv.FieldByIndex(f.Index).Interface()
		switch col {
		case "id":
			if s, _ := val.(string); s == "" {
				val = uuid.New().String()
				if fv := sv.FieldByIndex(f.Index); fv.CanSet() {
					fv.SetString(val.(string))
				}
			}
		case "created_at", "updated_at":
			val = now
		default:
			// PATCH-on-create: skip columns the request did not provide so the
			// DB applies its own DEFAULT/NULL, exactly like the map path.
			if present != nil {
				if _, ok := present[col]; !ok {
					continue
				}
			}
		}
		cols = append(cols, q(col))
		phs = append(phs, p.add(norm(val)))
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		q(model.TableName), strings.Join(cols, ", "), strings.Join(phs, ", "))
	return query, p.args
}

func buildUpdateSQL(model *maniflex.ModelMeta, id string, ptr any, present map[string]struct{}, driver maniflex.DriverType, norm func(any) any) (string, []any) {
	sv := reflect.ValueOf(ptr).Elem()
	now := time.Now().UTC()
	p := &ph{driver: driver}

	sets := make([]string, 0, len(present)+1)
	for i := range model.Fields {
		f := &model.Fields[i]
		col := f.Tags.DBName
		if col == "id" || col == "updated_at" {
			continue
		}
		if _, ok := present[col]; !ok {
			continue
		}
		val := sv.FieldByIndex(f.Index).Interface()
		sets = append(sets, fmt.Sprintf("%s = %s", q(col), p.add(norm(val))))
	}
	if model.FieldByDBName("updated_at") != nil {
		sets = append(sets, fmt.Sprintf("%s = %s", q("updated_at"), p.add(norm(now))))
	}

	where := fmt.Sprintf("%s = %s", q("id"), p.add(id))
	if c := softDeleteCond(model, model.TableName, driver); c != "" {
		where += " AND " + c
	}

	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s",
		q(model.TableName), strings.Join(sets, ", "), where)
	return query, p.args
}
