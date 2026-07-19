package e2e

// Audit MS-9: relation includes fetched related rows with only the soft-delete
// condition, so a tenancy or force-filter scope constrained the primary read and
// stopped at the relation boundary.
//
// It is exploitable in both directions. A caller who can set a foreign key puts
// their own row inside another tenant's ?include= (verified: a child belonging
// to tenant-b appeared in tenant-a's response), and a many-to-many attach pulls
// another tenant's record into the attacker's own response — for which the
// framework provides no junction validation at all.
//
//	go test ./tests/e2e/... -run TestIncludeScope

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type incParent struct {
	maniflex.BaseModel
	OrgID string     `json:"org_id" db:"org_id" mfx:"filterable"`
	Title string     `json:"title"  db:"title"`
	Kids  []incChild `json:"kids"`
}

type incChild struct {
	maniflex.BaseModel
	OrgID       string `json:"org_id"        db:"org_id" mfx:"filterable"`
	Secret      string `json:"secret"        db:"secret"`
	IncParentID string `json:"inc_parent_id" db:"inc_parent_id" mfx:"filterable,relation"`
}

// incLookup is the shared, unpartitioned table the fix deliberately leaves
// alone: it carries no tenant column, so there is nothing to scope it by.
type IncLookup struct {
	maniflex.BaseModel
	Code string `json:"code" db:"code"`
}

type incOrder struct {
	maniflex.BaseModel
	OrgID       string `json:"org_id"        db:"org_id" mfx:"filterable"`
	Name        string `json:"name"          db:"name"`
	IncLookupID string `json:"inc_lookup_id" db:"inc_lookup_id" mfx:"filterable,relation"`
}

var (
	incA = map[string]string{"X-Org": "tenant-a"}
	incB = map[string]string{"X-Org": "tenant-b"}
)

func incServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{incParent{}, incChild{}, incOrder{}, IncLookup{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
					if o := ctx.Request.Header.Get("X-Org"); o != "" {
						return o
					}
					return "tenant-a"
				}),
				// Scoped to the models that carry the column — incLookup does not.
				maniflex.ForModel("incParent", "incChild", "incOrder"))
		},
	})
}

// The headline: a row planted by another tenant must not appear in an include.
func TestIncludeScope_ForeignRowIsNotIncluded(t *testing.T) {
	srv := incServer(t)

	pa := srv.MustID(srv.POST("/inc_parents", map[string]any{"title": "A-parent"}, incA))
	srv.POST("/inc_childs", map[string]any{"secret": "A-SECRET", "inc_parent_id": pa}, incA)

	// tenant-b points one of its own children at tenant-a's parent. Nothing
	// stops this: the FK is the client's to set, and the junction/FK write is
	// not validated by the framework.
	srv.POST("/inc_childs", map[string]any{"secret": "B-PLANTED", "inc_parent_id": pa}, incB)

	body := string(srv.GET("/inc_parents/"+pa+"?include=kids", incA).Body)
	if strings.Contains(body, "B-PLANTED") {
		t.Errorf("tenant-a's include returned a row owned by tenant-b: %s", body)
	}
	if !strings.Contains(body, "A-SECRET") {
		t.Errorf("the owner's own child must still be included: %s", body)
	}
	if strings.Contains(body, "tenant-b") {
		t.Errorf("no tenant-b row should appear anywhere in tenant-a's response: %s", body)
	}
}

// The other direction: the include must not become an exfiltration path either.
func TestIncludeScope_ForeignRowIsNotReadableViaInclude(t *testing.T) {
	srv := incServer(t)

	pa := srv.MustID(srv.POST("/inc_parents", map[string]any{"title": "A-parent"}, incA))
	srv.POST("/inc_childs", map[string]any{"secret": "A-SECRET", "inc_parent_id": pa}, incA)

	// tenant-b cannot read the parent at all, so the include cannot be reached
	// through it — the 404 is the same one the record gives.
	srv.GET("/inc_parents/"+pa+"?include=kids", incB).AssertStatus(404)

	// And tenant-b's own parent shows only tenant-b's children.
	pb := srv.MustID(srv.POST("/inc_parents", map[string]any{"title": "B-parent"}, incB))
	srv.POST("/inc_childs", map[string]any{"secret": "B-OWN", "inc_parent_id": pb}, incB)

	body := string(srv.GET("/inc_parents/"+pb+"?include=kids", incB).Body)
	if !strings.Contains(body, "B-OWN") {
		t.Errorf("tenant-b must see their own child: %s", body)
	}
	if strings.Contains(body, "A-SECRET") {
		t.Errorf("tenant-b's include leaked tenant-a's child: %s", body)
	}
}

// A related model with no tenant column is left unscoped, deliberately: a shared
// lookup table is not partitioned and has nothing to scope by. Without this the
// fix could scope everything into oblivion and still pass the tests above.
func TestIncludeScope_UnpartitionedLookupStillIncludes(t *testing.T) {
	srv := incServer(t)

	lid := srv.MustID(srv.POST("/inc_lookups", map[string]any{"code": "EUR"}, incA))
	oid := srv.MustID(srv.POST("/inc_orders",
		map[string]any{"name": "order-b", "inc_lookup_id": lid}, incB))

	body := string(srv.GET("/inc_orders/"+oid+"?include=inc_lookup", incB).Body)
	if !strings.Contains(body, "EUR") {
		t.Errorf("a shared lookup row has no tenant column and must still be "+
			"included: %s", body)
	}
}

// The unscoped case must stay unscoped only because the column is absent — an
// include of a model that DOES carry the column is scoped even when the parent
// row was reached by id.
func TestIncludeScope_ListIncludesAreScopedToo(t *testing.T) {
	srv := incServer(t)

	pa := srv.MustID(srv.POST("/inc_parents", map[string]any{"title": "A-parent"}, incA))
	srv.POST("/inc_childs", map[string]any{"secret": "A-SECRET", "inc_parent_id": pa}, incA)
	srv.POST("/inc_childs", map[string]any{"secret": "B-PLANTED", "inc_parent_id": pa}, incB)

	body := string(srv.GET("/inc_parents?include=kids", incA).Body)
	if strings.Contains(body, "B-PLANTED") {
		t.Errorf("a list include leaked another tenant's row: %s", body)
	}
	if !strings.Contains(body, "A-SECRET") {
		t.Errorf("the owner's own child must still be included: %s", body)
	}
}
