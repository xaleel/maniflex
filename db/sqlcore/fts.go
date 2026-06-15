package sqlcore

// fts.go — per-model full-text search (roadmap 4.9).
//
// When QueryParams.Search is set (the ?q= parameter on a model with
// mfx:"searchable" fields) the list query gains a native FTS predicate over the
// searchable columns and orders results by match relevance instead of the
// default column ordering. The two backends use their native machinery:
//
//   - Postgres: a STORED generated `fts` tsvector column over the searchable
//     columns plus a GIN index (provisioned by migrateFTS). Matching uses
//     `fts @@ websearch_to_tsquery(lang, q)`; ranking uses ts_rank. The generated
//     column self-maintains on every write, so no triggers are needed.
//   - SQLite: an external-content FTS5 shadow table `<table>_fts` kept in sync by
//     AFTER INSERT/UPDATE/DELETE triggers. Matching joins the shadow table on
//     rowid and uses `<table>_fts MATCH q`; ranking uses FTS5's bm25 `rank`.
//
// Soft-deleted rows stay in the index but are excluded from results by the
// base-table soft-delete predicate that allWhereConds already adds.

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"maniflex"
)

const (
	// ftsColumnPG is the generated tsvector column added to Postgres tables that
	// declare searchable fields. SQLite keeps its index in a separate shadow
	// table (ftsTableName) instead, so this column never appears there.
	ftsColumnPG = "fts"
	// defaultSearchLang is the Postgres text-search configuration used when the
	// model does not set ModelConfig.SearchLanguage.
	defaultSearchLang = "english"
)

// ftsTableName returns the name of the SQLite FTS5 shadow table for a base table.
func ftsTableName(table string) string { return table + "_fts" }

// searchLang returns the Postgres text-search configuration name for a model,
// defaulting to "english". collectSearchFields validated it is a plain
// identifier, so embedding it directly in SQL is safe.
func searchLang(model *maniflex.ModelMeta) string {
	if model.Config.SearchLanguage != "" {
		return model.Config.SearchLanguage
	}
	return defaultSearchLang
}

// ftsActive reports whether qp requests a full-text search the model supports.
func ftsActive(model *maniflex.ModelMeta, qp *maniflex.QueryParams) bool {
	return qp != nil && qp.Search != "" && len(model.SearchFields) > 0
}

// ftsJoinSQL returns the JOIN that brings FTS ranking into a list query: SQLite
// joins the shadow FTS5 table on rowid; Postgres needs none (its tsvector column
// lives on the base table). Empty when no search is active.
func ftsJoinSQL(model *maniflex.ModelMeta, qp *maniflex.QueryParams, driver maniflex.DriverType) string {
	if !ftsActive(model, qp) || driver == maniflex.Postgres {
		return ""
	}
	ft := ftsTableName(model.TableName)
	return fmt.Sprintf(" JOIN %s ON %s.%s = %s.%s",
		q(ft), q(ft), q("rowid"), q(model.TableName), q("rowid"))
}

// appendSearchCond appends the full-text MATCH/@@ predicate to conds (binding the
// query argument to p) when a search is active, else returns conds unchanged.
func appendSearchCond(conds []string, model *maniflex.ModelMeta, qp *maniflex.QueryParams, driver maniflex.DriverType, p *ph) []string {
	if !ftsActive(model, qp) {
		return conds
	}
	if driver == maniflex.Postgres {
		// websearch_to_tsquery never errors on user input — no sanitisation needed.
		return append(conds, fmt.Sprintf("%s.%s @@ websearch_to_tsquery('%s', %s)",
			q(model.TableName), q(ftsColumnPG), searchLang(model), p.add(qp.Search)))
	}
	match := sqliteMatchQuery(qp.Search)
	if match == "" {
		// Input had no usable tokens (only punctuation) — match nothing.
		return append(conds, "0")
	}
	return append(conds, fmt.Sprintf("%s MATCH %s",
		q(ftsTableName(model.TableName)), p.add(match)))
}

// listOrderSQL returns the ORDER BY for a non-cursor list query. When a search is
// active, match relevance is the primary key, followed by any requested column
// sorts as tiebreakers; otherwise it is the plain column ordering. p is used for
// the Postgres ts_rank bind, so it must already hold the WHERE args.
func listOrderSQL(model *maniflex.ModelMeta, qp *maniflex.QueryParams, driver maniflex.DriverType, p *ph) string {
	base := buildOrder(model, qp.Sorts, driver)
	if !ftsActive(model, qp) {
		return base
	}
	var rel string
	if driver == maniflex.Postgres {
		rel = fmt.Sprintf("ts_rank(%s.%s, websearch_to_tsquery('%s', %s)) DESC",
			q(model.TableName), q(ftsColumnPG), searchLang(model), p.add(qp.Search))
	} else {
		// FTS5 bm25 rank: lower is more relevant, so ascending order is best-first.
		rel = q(ftsTableName(model.TableName)) + "." + q("rank")
	}
	if base == "" {
		return " ORDER BY " + rel
	}
	return " ORDER BY " + rel + ", " + strings.TrimPrefix(base, " ORDER BY ")
}

// sqliteMatchQuery converts free-form user input into a safe FTS5 MATCH query:
// each run of word characters becomes a double-quoted phrase term, and the terms
// are ANDed (FTS5's implicit AND). Quoting neutralises FTS5 operators so the
// query can never be a syntax error. Returns "" when the input has no usable
// tokens.
func sqliteMatchQuery(s string) string {
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			terms = append(terms, `"`+b.String()+`"`)
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return strings.Join(terms, " ")
}

// ── Migration ─────────────────────────────────────────────────────────────────

// migrateFTS provisions the driver's native full-text search index for a model
// that declares mfx:"searchable" fields. Idempotent — safe on every startup.
func (a *Adapter) migrateFTS(ctx context.Context, exec sqlExec, m *maniflex.ModelMeta) error {
	if len(m.SearchFields) == 0 {
		return nil
	}
	if a.driver == maniflex.Postgres {
		return a.migrateFTSPostgres(ctx, exec, m)
	}
	return a.migrateFTSSQLite(ctx, exec, m)
}

// migrateFTSPostgres adds the STORED generated tsvector column and its GIN index.
// The generated column keeps itself in sync on every write — no triggers.
func (a *Adapter) migrateFTSPostgres(ctx context.Context, exec sqlExec, m *maniflex.ModelMeta) error {
	parts := make([]string, len(m.SearchFields))
	for i, c := range m.SearchFields {
		parts[i] = fmt.Sprintf("coalesce(%s, '')", q(c))
	}
	expr := fmt.Sprintf("to_tsvector('%s', %s)", searchLang(m), strings.Join(parts, " || ' ' || "))

	addCol := fmt.Sprintf(
		"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s tsvector GENERATED ALWAYS AS (%s) STORED",
		q(m.TableName), q(ftsColumnPG), expr)
	if _, err := exec.ExecContext(ctx, addCol); err != nil {
		return fmt.Errorf("add fts column: %w", err)
	}

	idx := fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s USING GIN (%s)",
		"idx_"+m.TableName+"_fts", q(m.TableName), q(ftsColumnPG))
	if _, err := exec.ExecContext(ctx, idx); err != nil {
		return fmt.Errorf("create fts index: %w", err)
	}
	return nil
}

// migrateFTSSQLite creates the external-content FTS5 shadow table and the three
// sync triggers, seeding the index from existing rows only on first creation.
func (a *Adapter) migrateFTSSQLite(ctx context.Context, exec sqlExec, m *maniflex.ModelMeta) error {
	ftsTable := ftsTableName(m.TableName)
	exists, err := sqliteObjectExists(ctx, exec, ftsTable)
	if err != nil {
		return err
	}

	colsCSV := strings.Join(quoteAll(m.SearchFields), ", ")

	// Note: the shadow table's column set is fixed at creation. If the set of
	// searchable fields changes later, the CREATE VIRTUAL TABLE IF-NOT-EXISTS
	// below is a no-op; drop <table>_fts manually to rebuild with new columns.
	if !exists {
		create := fmt.Sprintf(
			"CREATE VIRTUAL TABLE %s USING fts5(%s, content=%s, content_rowid='rowid', tokenize='porter unicode61')",
			q(ftsTable), colsCSV, sqlStringLiteral(m.TableName))
		if _, err := exec.ExecContext(ctx, create); err != nil {
			return fmt.Errorf("create fts5 table: %w", err)
		}
	}

	newVals := triggerValues(m.SearchFields, "new")
	oldVals := triggerValues(m.SearchFields, "old")
	triggers := []string{
		fmt.Sprintf(
			"CREATE TRIGGER IF NOT EXISTS %s AFTER INSERT ON %s BEGIN "+
				"INSERT INTO %s(rowid, %s) VALUES (new.rowid, %s); END",
			q(m.TableName+"_fts_ai"), q(m.TableName), q(ftsTable), colsCSV, newVals),
		fmt.Sprintf(
			"CREATE TRIGGER IF NOT EXISTS %s AFTER DELETE ON %s BEGIN "+
				"INSERT INTO %s(%s, rowid, %s) VALUES ('delete', old.rowid, %s); END",
			q(m.TableName+"_fts_ad"), q(m.TableName), q(ftsTable), q(ftsTable), colsCSV, oldVals),
		fmt.Sprintf(
			"CREATE TRIGGER IF NOT EXISTS %s AFTER UPDATE ON %s BEGIN "+
				"INSERT INTO %s(%s, rowid, %s) VALUES ('delete', old.rowid, %s); "+
				"INSERT INTO %s(rowid, %s) VALUES (new.rowid, %s); END",
			q(m.TableName+"_fts_au"), q(m.TableName), q(ftsTable), q(ftsTable), colsCSV, oldVals,
			q(ftsTable), colsCSV, newVals),
	}
	for _, tg := range triggers {
		if _, err := exec.ExecContext(ctx, tg); err != nil {
			return fmt.Errorf("create fts trigger: %w", err)
		}
	}

	if !exists {
		// Seed from existing rows once; the triggers maintain it thereafter.
		rebuild := fmt.Sprintf("INSERT INTO %s(%s) VALUES ('rebuild')", q(ftsTable), q(ftsTable))
		if _, err := exec.ExecContext(ctx, rebuild); err != nil {
			return fmt.Errorf("rebuild fts index: %w", err)
		}
	}
	return nil
}

// isFTSColumn reports whether col is the framework-managed Postgres generated
// tsvector column for a searchable model, so AutoMigrate's drift check ignores
// it instead of warning that the model declares no such field.
func isFTSColumn(col string, m *maniflex.ModelMeta) bool {
	return len(m.SearchFields) > 0 && col == ftsColumnPG
}

// sqliteObjectExists reports whether a table (including a virtual table) named
// name exists in the SQLite schema.
func sqliteObjectExists(ctx context.Context, exec sqlExec, name string) (bool, error) {
	rows, err := exec.QueryContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := rows.Next()
	return found, rows.Err()
}

// quoteAll quotes each identifier in cols.
func quoteAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = q(c)
	}
	return out
}

// triggerValues renders the per-column value list for an FTS sync trigger, e.g.
// `new."title", new."body"`.
func triggerValues(cols []string, ref string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = ref + "." + q(c)
	}
	return strings.Join(out, ", ")
}

// sqlStringLiteral renders s as a single-quoted SQL string literal.
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
