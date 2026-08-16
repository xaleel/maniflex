package e2e

// Audit O2 — the include scope resolved filter fields by DB name only, while
// every other consumer of a filter resolves either spelling.
//
// maniflex.BuildFilterSQL → filterCond calls ModelMeta.ResolveFilterField, which
// accepts the DB name or the json name. includeScopeCond called
// ModelMeta.FieldByDBName and skipped anything it could not resolve. So a
// tenancy scope declared with the json spelling constrained the primary read and
// was silently dropped on the include — the exact MS-9 leak, reopened for one
// spelling of the same configuration.
//
// The json spelling is not an exotic way to write this. db.Tenancy passes its
// field straight to ctx.SetField, whose parameter is named jsonName and is
// documented as the json name, so the write path wants the json spelling while
// the include quietly required the DB one.
//
//	go test ./tests/e2e/... -run TestIncludeScopeSpelling

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// The tenant column's two spellings differ, which is what the bug turns on: with
// json:"org_id" db:"org_id" (as the MS-9 fixtures have it) FieldByDBName happens
// to resolve the json name too, and the gap cannot be seen.
type spellParent struct {
	maniflex.BaseModel
	OrgID string       `json:"orgId" db:"org_id" mfx:"filterable"`
	Title string       `json:"title" db:"title"`
	Kids  []spellChild `json:"kids"`
}

type spellChild struct {
	maniflex.BaseModel
	OrgID         string `json:"orgId"         db:"org_id"          mfx:"filterable"`
	Secret        string `json:"secret"        db:"secret"`
	SpellParentID string `json:"spellParentId" db:"spell_parent_id" mfx:"filterable,relation"`
}

var (
	spellA = map[string]string{"X-Org": "tenant-a"}
	spellB = map[string]string{"X-Org": "tenant-b"}
)

// spellServer scopes by the *json* spelling of the tenant column.
func spellServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{spellParent{}, spellChild{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.Tenancy("orgId", func(ctx *maniflex.ServerContext) string {
					if o := ctx.Request.Header.Get("X-Org"); o != "" {
						return o
					}
					return "tenant-a"
				}),
				maniflex.ForModel("spellParent", "spellChild"))
		},
	})
}

// The headline, and the same one MS-9 answered for the DB spelling: a row
// planted by another tenant must not appear in an include.
func TestIncludeScopeSpelling_JSONNamedScopeAppliesToIncludes(t *testing.T) {
	srv := spellServer(t)

	pa := srv.MustID(srv.POST("/spell_parents", map[string]any{"title": "A-parent"}, spellA))
	srv.POST("/spell_childs", map[string]any{"secret": "A-SECRET", "spellParentId": pa}, spellA)

	// tenant-b points one of its own children at tenant-a's parent — the FK is
	// the client's to set, exactly as in the MS-9 scenario.
	srv.POST("/spell_childs", map[string]any{"secret": "B-PLANTED", "spellParentId": pa}, spellB)

	body := string(srv.GET("/spell_parents/"+pa+"?include=kids", spellA).Body)

	if strings.Contains(body, "B-PLANTED") {
		t.Errorf("tenant-a's include returned a row owned by tenant-b — the tenancy scope "+
			"was declared with the json spelling of the column, applied to the primary "+
			"read, and dropped at the relation boundary:\n%s", body)
	}
	if !strings.Contains(body, "A-SECRET") {
		t.Errorf("the owner's own child must still be included:\n%s", body)
	}
	if strings.Contains(body, "tenant-b") {
		t.Errorf("no tenant-b value should appear anywhere in tenant-a's response:\n%s", body)
	}
}

// The control. The same scope, the same spelling, on the primary read: this
// already worked, and it is what makes the include gap silent — the
// configuration looks like it is holding right up until a relation is included.
func TestIncludeScopeSpelling_JSONNamedScopeAppliesToThePrimaryRead(t *testing.T) {
	srv := spellServer(t)

	pa := srv.MustID(srv.POST("/spell_parents", map[string]any{"title": "A-parent"}, spellA))

	body := string(srv.GET("/spell_parents", spellB).Body)
	if strings.Contains(body, pa) || strings.Contains(body, "A-parent") {
		t.Errorf("tenant-b's list returned tenant-a's parent, so the json-spelled scope "+
			"is not applying even on the primary read:\n%s", body)
	}
}
