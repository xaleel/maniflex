package maniflex

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The cascade's child lookup walked a soft-delete edge with LIMIT/OFFSET and no
// ORDER BY, accumulating every matching row — full column maps, not ids — before
// acting on any of them (audit O9).
//
// Two consequences, one of which the audit does not name:
//
//   - A SELECT with no ORDER BY may return rows in any order, and nothing
//     obliges a database to pick the same order for the next OFFSET. Postgres
//     genuinely does not: it costs OFFSET into the plan, and a synchronised
//     sequential scan starts wherever another scan happens to be. Paging by
//     offset over an unordered result therefore skips rows — and a skipped row
//     on a cascade edge is a child left pointing at a parent that no longer
//     exists.
//   - The accumulation is unbounded and reads columns nobody looks at. Only the
//     id of each row is ever used, and onDelete:restrict uses only the count.
//
// cascadeFake is a conforming database for the purposes of these tests: it
// honours the filters, the limit and the offset, and it orders the result set
// only when the query asks it to. When the query does not, it exercises the
// freedom SQL grants it and hands back a different order each time.
type cascadeFake struct {
	DBAdapter
	rows   []map[string]any // the child table, ascending by id
	nulled map[string]bool
	gone   map[string]bool

	events  []string // "find" / "update" / "delete", in the order they happened
	queries []*QueryParams
	finds   int
	served  int // rows handed to the cascade across every query

	// noCount models an adapter whose FindMany does not report a total, the way
	// sqlcore's own cursor mode does not. The DBAdapter contract asks for one, so
	// this is a misbehaving adapter — but restrict is a guard, and a guard must
	// not be talked out of firing by a number it only needed for the message.
	noCount bool
}

func newCascadeFake(n int) *cascadeFake {
	f := &cascadeFake{nulled: map[string]bool{}, gone: map[string]bool{}}
	for i := range n {
		f.rows = append(f.rows, map[string]any{
			"id":        fmt.Sprintf("child-%05d", i),
			"parent_id": "p1",
			"payload":   strings.Repeat("x", 32),
		})
	}
	return f
}

func (f *cascadeFake) FindMany(_ context.Context, _ *ModelMeta, qp *QueryParams) ([]any, int64, error) {
	f.events = append(f.events, "find")
	f.queries = append(f.queries, qp)
	f.finds++

	var match []map[string]any
	for _, r := range f.rows {
		id := r["id"].(string)
		if f.gone[id] || f.nulled[id] {
			continue
		}
		if f.matches(qp.Filters, r) {
			match = append(match, r)
		}
	}
	if sortsByIDAsc(qp.Sorts) {
		sort.Slice(match, func(i, j int) bool {
			return match[i]["id"].(string) < match[j]["id"].(string)
		})
	} else {
		// No ORDER BY: the order is unspecified, and a real planner does not
		// promise the same one twice. Rotate by the call number.
		match = rotateRows(match, f.finds*7)
	}
	total := int64(len(match))
	if f.noCount {
		total = -1
	}

	off := min(qp.Offset(), len(match))
	end := len(match)
	if qp.Limit > 0 && off+qp.Limit < end {
		end = off + qp.Limit
	}
	page := match[off:end]
	f.served += len(page)
	out := make([]any, len(page))
	for i, r := range page {
		out[i] = r
	}
	return out, total, nil
}

func (f *cascadeFake) matches(filters []*FilterExpr, row map[string]any) bool {
	for _, fe := range filters {
		got := fmt.Sprint(row[fe.Field])
		want := fmt.Sprint(fe.Value)
		switch fe.Operator {
		case OpEq:
			if got != want {
				return false
			}
		case OpGt:
			if got <= want {
				return false
			}
		default:
			panic("cascadeFake: unhandled operator " + string(fe.Operator))
		}
	}
	return true
}

func (f *cascadeFake) Update(_ context.Context, _ *ModelMeta, id string, _ any, _ map[string]struct{}) (any, error) {
	f.events = append(f.events, "update")
	f.nulled[id] = true
	return map[string]any{"id": id}, nil
}

func (f *cascadeFake) Delete(_ context.Context, _ *ModelMeta, id string) error {
	f.events = append(f.events, "delete")
	f.gone[id] = true
	return nil
}

func sortsByIDAsc(sorts []SortExpr) bool {
	return len(sorts) > 0 && sorts[0].DBName == "id" && sorts[0].Direction == SortAsc
}

func rotateRows(rows []map[string]any, by int) []map[string]any {
	if len(rows) == 0 {
		return rows
	}
	by %= len(rows)
	return append(append([]map[string]any{}, rows[by:]...), rows[:by]...)
}

// cascadeSetup registers a soft-deleting parent and a child holding an onDelete
// edge back to it, and returns everything cascadeChildren needs. The parent soft
// deletes so the edge is enforced in the maniflex layer rather than by a database
// FK constraint — the path under test.
func cascadeSetup(t *testing.T, f *cascadeFake, action OnDeleteAction) (*defaultSteps, *ServerContext, dbExec, *ModelMeta) {
	t.Helper()
	reg := NewRegistry()
	parent := &ModelMeta{
		Name:       "CascParent",
		TableName:  "casc_parents",
		SoftDelete: SoftDeleteConfig{Enabled: true, Field: "deleted_at"},
	}
	mustAdd(t, reg, parent)
	mustAdd(t, reg, &ModelMeta{
		Name:      "CascChild",
		TableName: "casc_children",
		Relations: []RelationMeta{belongsTo("CascParent", "parent_id", "parent", action)},
	})
	return &defaultSteps{reg: reg, adapter: f},
		&ServerContext{Ctx: context.Background()},
		dbExec{adapter: f},
		parent
}

// A fan-out larger than one page must be walked without losing rows. Every child
// is reachable; every child must be nulled.
func TestCascadeChildLookupLosesNoRowsAcrossPages(t *testing.T) {
	const n = 1200
	f := newCascadeFake(n)
	s, ctx, exec, parent := cascadeSetup(t, f, OnDeleteSetNull)

	if err := s.cascadeChildren(ctx, exec, parent, "p1", map[string]bool{}); err != nil {
		t.Fatalf("cascadeChildren: %v", err)
	}
	var missed []string
	for _, r := range f.rows {
		if id := r["id"].(string); !f.nulled[id] {
			missed = append(missed, id)
		}
	}
	if len(missed) != 0 {
		t.Errorf("%d of %d children were never nulled — offset paging over an "+
			"unordered result set skipped them; each is left pointing at a deleted "+
			"parent. First few: %v", len(missed), n, missed[:min(5, len(missed))])
	}
}

// The whole fan-out must not be held at once. A snapshot-then-act cascade reads
// every page before it writes anything; a streaming one interleaves.
func TestCascadeChildLookupDoesNotHoldTheWholeFanOut(t *testing.T) {
	f := newCascadeFake(1200)
	s, ctx, exec, parent := cascadeSetup(t, f, OnDeleteSetNull)

	if err := s.cascadeChildren(ctx, exec, parent, "p1", map[string]bool{}); err != nil {
		t.Fatalf("cascadeChildren: %v", err)
	}
	firstWrite := indexOfEvent(f.events, "update")
	lastRead := lastIndexOfEvent(f.events, "find")
	if firstWrite < 0 {
		t.Fatal("no child was nulled")
	}
	if firstWrite > lastRead {
		t.Errorf("every page was read before any child was written (%d reads, first "+
			"write at %d): the cascade holds the whole fan-out in memory",
			f.finds, firstWrite)
	}
}

// Only the id of a child row is ever used. Reading whole rows to throw the
// columns away is what makes a large fan-out expensive.
func TestCascadeChildLookupReadsOnlyTheIDColumn(t *testing.T) {
	f := newCascadeFake(600)
	s, ctx, exec, parent := cascadeSetup(t, f, OnDeleteCascade)

	if err := s.cascadeChildren(ctx, exec, parent, "p1", map[string]bool{}); err != nil {
		t.Fatalf("cascadeChildren: %v", err)
	}
	for i, qp := range f.queries {
		if len(qp.Fields) != 1 || qp.Fields[0] != "id" {
			t.Errorf("query %d selects %v, want only the id column", i, qp.Fields)
		}
	}
}

// restrict refuses the delete and reports how many children still reference the
// parent. It needs the count, not the rows — and restrict is precisely the edge
// where a large fan-out is expected, since that is why it is declared.
func TestCascadeRestrictCountsWithoutLoadingTheFanOut(t *testing.T) {
	const n = 5000
	f := newCascadeFake(n)
	s, ctx, exec, parent := cascadeSetup(t, f, OnDeleteRestrict)

	err := s.cascadeChildren(ctx, exec, parent, "p1", map[string]bool{})
	if err == nil {
		t.Fatal("cascadeChildren allowed a delete a restrict edge should refuse")
	}
	if f.finds != 1 {
		t.Errorf("restrict issued %d queries for %d children; the count is one query", f.finds, n)
	}
	if f.served > 1 {
		t.Errorf("restrict read %d of the %d referencing rows; it needs to know that "+
			"one exists and how many there are, not what they contain", f.served, n)
	}
	if ctx.Response == nil || ctx.Response.StatusCode != 409 {
		t.Fatalf("response = %+v, want a 409", ctx.Response)
	}
	if msg := ctx.Response.Error.Message; !strings.Contains(msg, fmt.Sprint(n)) {
		t.Errorf("409 message %q does not report the %d referencing records", msg, n)
	}
}

// An adapter that does not count must not turn the refusal into a nonsense
// message. The rows decide; the count only says how many.
func TestCascadeRestrictSurvivesAnAdapterThatDoesNotCount(t *testing.T) {
	f := newCascadeFake(7)
	f.noCount = true
	s, ctx, exec, parent := cascadeSetup(t, f, OnDeleteRestrict)

	if err := s.cascadeChildren(ctx, exec, parent, "p1", map[string]bool{}); err == nil {
		t.Fatal("cascadeChildren allowed a delete a restrict edge should refuse")
	}
	if ctx.Response == nil || ctx.Response.StatusCode != 409 {
		t.Fatalf("response = %+v, want a 409", ctx.Response)
	}
	msg := ctx.Response.Error.Message
	if strings.Contains(msg, "-1") || strings.Contains(msg, " 0 ") {
		t.Errorf("409 message reports an impossible count: %q", msg)
	}
}

func indexOfEvent(events []string, want string) int {
	for i, e := range events {
		if e == want {
			return i
		}
	}
	return -1
}

func lastIndexOfEvent(events []string, want string) int {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i] == want {
			return i
		}
	}
	return -1
}
