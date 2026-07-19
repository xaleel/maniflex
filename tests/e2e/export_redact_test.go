package e2e

// Audit MS-11: an export streams bytes and never builds a ctx.Response, so a
// Response-step middleware that masks a field by mutating it silently masked
// nothing. An app that hid salary from non-admins on the JSON path served it in
// full at /employees/export — verified: the CSV contained "alice,123456" while
// the JSON list omitted the column.
//
// response.RedactField now declares the field before calling next(), and both
// the marshalling paths and the export honour the declaration. The export stays
// row-at-a-time; nothing is materialised to filter it.
//
//	go test ./tests/e2e/... -run TestExportRedact

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	resp "github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type xpEmployee struct {
	maniflex.BaseModel
	Name   string `json:"name"   db:"name"`
	Salary int    `json:"salary" db:"salary"`
}

// xpServer redacts salary when the caller is not an admin, which is decided per
// request — the thing a static mfx: tag cannot express.
func xpServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{xpEmployee{}, maniflex.ModelConfig{ExportEnabled: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				resp.RedactField("salary", func(ctx *maniflex.ServerContext) bool {
					return ctx.Request.Header.Get("X-Role") != "admin"
				}),
				maniflex.ForModel("xpEmployee"))
		},
	})
}

var (
	xpAdmin = map[string]string{"X-Role": "admin"}
	xpStaff = map[string]string{"X-Role": "staff"}
)

func xpSeed(t *testing.T, srv *testutil.Server) {
	t.Helper()
	srv.POST("/xp_employees", map[string]any{"name": "alice", "salary": 123456})
}

// The headline: the same registration that masks the JSON masks the export.
func TestExportRedact_ExportHonoursResponseRedaction(t *testing.T) {
	srv := xpServer(t)
	xpSeed(t, srv)

	csv := string(srv.GET("/xp_employees/export?format=csv", xpStaff).Body)
	if strings.Contains(csv, "123456") {
		t.Errorf("export leaked the redacted value: %q", csv)
	}
	// The column is dropped from the header too — an export should not advertise
	// a field it will not fill.
	if strings.Contains(csv, "salary") {
		t.Errorf("export header still advertises the redacted column: %q", csv)
	}
	if !strings.Contains(csv, "alice") {
		t.Errorf("the rest of the row must survive: %q", csv)
	}
}

// The decision is per request, so a caller the predicate allows still gets it.
// Without this the fix could redact unconditionally and pass the test above.
func TestExportRedact_PermittedCallerStillGetsTheColumn(t *testing.T) {
	srv := xpServer(t)
	xpSeed(t, srv)

	csv := string(srv.GET("/xp_employees/export?format=csv", xpAdmin).Body)
	if !strings.Contains(csv, "123456") {
		t.Errorf("an admin's export must still carry the value: %q", csv)
	}
	if !strings.Contains(csv, "salary") {
		t.Errorf("an admin's export must still carry the column: %q", csv)
	}
}

// The JSON path must keep behaving as it did — the declaration is now what
// drives it, so a regression there would be invisible from the export tests.
func TestExportRedact_JSONPathStillRedacts(t *testing.T) {
	srv := xpServer(t)
	xpSeed(t, srv)

	staff := string(srv.GET("/xp_employees", xpStaff).Body)
	if strings.Contains(staff, "123456") || strings.Contains(staff, "salary") {
		t.Errorf("JSON list leaked the redacted field: %s", staff)
	}
	admin := string(srv.GET("/xp_employees", xpAdmin).Body)
	if !strings.Contains(admin, "123456") {
		t.Errorf("an admin's JSON list must carry the value: %s", admin)
	}

	// Single reads take the same path.
	id := srv.MustID(srv.POST("/xp_employees", map[string]any{"name": "bob", "salary": 999}))
	one := string(srv.GET("/xp_employees/"+id, xpStaff).Body)
	if strings.Contains(one, "999") || strings.Contains(one, "salary") {
		t.Errorf("single read leaked the redacted field: %s", one)
	}
}

// XLSX goes through the same column list, so it must mask too. A format-specific
// fix would pass the CSV tests and leak here.
func TestExportRedact_XLSXHonoursRedactionToo(t *testing.T) {
	srv := xpServer(t)
	xpSeed(t, srv)

	// XLSX is a zip; the shared-strings part holds the cell text, so a leaked
	// value would appear in the raw bytes.
	body := string(srv.GET("/xp_employees/export?format=xlsx", xpStaff).Body)
	if strings.Contains(body, "123456") {
		t.Error("xlsx export leaked the redacted value")
	}
	if len(body) == 0 {
		t.Error("xlsx export produced no bytes")
	}
}

// ctx.RedactResponseField is public, so a hand-written middleware may declare a
// field without also editing ctx.Response. The declaration alone must mask both
// shapes — otherwise the API would mask the CSV and quietly leave the JSON, the
// mirror image of the bug this fixes.
func TestExportRedact_DeclarationAloneMasksBothShapes(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{xpEmployee{}, maniflex.ModelConfig{ExportEnabled: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Response.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Request.Header.Get("X-Role") != "admin" {
						ctx.RedactResponseField("salary")
					}
					return next()
				}, maniflex.ForModel("xpEmployee"))
		},
	})
	xpSeed(t, srv)

	jsonBody := string(srv.GET("/xp_employees", xpStaff).Body)
	if strings.Contains(jsonBody, "123456") || strings.Contains(jsonBody, "salary") {
		t.Errorf("a declared field must be masked in JSON without touching "+
			"ctx.Response: %s", jsonBody)
	}
	csv := string(srv.GET("/xp_employees/export?format=csv", xpStaff).Body)
	if strings.Contains(csv, "123456") || strings.Contains(csv, "salary") {
		t.Errorf("a declared field must be masked in the export: %q", csv)
	}
	// And an admin, who declares nothing, still sees it in both.
	if !strings.Contains(string(srv.GET("/xp_employees", xpAdmin).Body), "123456") {
		t.Error("an admin's JSON must still carry the value")
	}
	if !strings.Contains(string(srv.GET("/xp_employees/export?format=csv", xpAdmin).Body), "123456") {
		t.Error("an admin's export must still carry the value")
	}
}
