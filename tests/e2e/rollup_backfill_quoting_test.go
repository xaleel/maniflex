package e2e

import (
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// BackfillRollups built its parent-discovery scan by interpolating the foreign
// key and the child table into the SQL unquoted — the one place in the codebase
// that did, while the migrator creates both quoted and every other statement
// quotes on the way back out (audit O6).
//
// Two spellings reach it, and they fail on different drivers, which is why both
// are here:
//
//   - A camelCase foreign key. json:/db: names are carried to the DB name
//     verbatim, so json:"parentId" is a column literally named parentId, created
//     case-sensitively. Postgres folds the unquoted reference to "parentid" and
//     cannot find it. SQLite folds identifier case and does not care, so this
//     half is invisible on the default local lane and waits for production.
//   - A reserved word. This one is a syntax error on both.

type bfCamelParent struct {
	maniflex.BaseModel
	Total float64 `json:"total"`
}

type bfCamelChild struct {
	maniflex.BaseModel
	Amount   float64 `json:"amount"`
	ParentID string  `json:"parentId"`
}

type bfReservedParent struct {
	maniflex.BaseModel
	Total float64 `json:"total"`
}

// Group names the parent this row belongs to. A foreign key spelt after its
// parent model rather than suffixed with _id is ordinary enough, and "group" is
// reserved on both drivers.
type bfReservedChild struct {
	maniflex.BaseModel
	Amount float64 `json:"amount"`
	Group  string  `json:"group"`
}

// Value is named for what its table becomes: toSnakeCase + pluralize makes it
// "values", which is reserved on both drivers. It covers the FROM half of the
// statement, which the two cases above cannot — a default table name is
// lowercase and unreserved, so quoting it is untested without a name like this
// one, and an untested guard is one a later edit can drop unnoticed.
type Value struct {
	maniflex.BaseModel
	Amount   float64 `json:"amount"`
	ParentID string  `json:"parent_id"`
}

func TestRollupBackfill_QuotesIdentifiers(t *testing.T) {
	t.Parallel()

	// No rows are needed: the scan fails on the statement, not on the data, so
	// an empty table exercises it exactly as a populated one would.
	t.Run("camelCase foreign key", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{bfCamelParent{}, bfCamelChild{}},
			Middleware: func(s *maniflex.Server) {
				s.MustRegisterRollup(maniflex.Rollup{
					Parent: "bfCamelParent", ParentField: "total", Op: maniflex.AggSum,
					Child: "bfCamelChild", ChildField: "amount", On: "parentId",
				})
			},
		})
		if err := srv.ManiflexServer().BackfillRollups(t.Context()); err != nil {
			t.Errorf("BackfillRollups: %v", err)
		}
	})

	t.Run("reserved word foreign key", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{bfReservedParent{}, bfReservedChild{}},
			Middleware: func(s *maniflex.Server) {
				s.MustRegisterRollup(maniflex.Rollup{
					Parent: "bfReservedParent", ParentField: "total", Op: maniflex.AggSum,
					Child: "bfReservedChild", ChildField: "amount", On: "group",
				})
			},
		})
		if err := srv.ManiflexServer().BackfillRollups(t.Context()); err != nil {
			t.Errorf("BackfillRollups: %v", err)
		}
	})
	t.Run("reserved word table name", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{bfReservedParent{}, Value{}},
			Middleware: func(s *maniflex.Server) {
				s.MustRegisterRollup(maniflex.Rollup{
					Parent: "bfReservedParent", ParentField: "total", Op: maniflex.AggSum,
					Child: "Value", ChildField: "amount", On: "parent_id",
				})
			},
		})
		if err := srv.ManiflexServer().BackfillRollups(t.Context()); err != nil {
			t.Errorf("BackfillRollups: %v", err)
		}
	})
}
