package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/admin"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// adminPanel builds a Server backed by in-memory SQLite, mounts the admin panel
// on it, and returns the panel handler plus the underlying server.
func adminPanel(t *testing.T, cfg admin.Config) (http.Handler, *maniflex.Server) {
	t.Helper()
	server := maniflex.New(maniflex.Config{PathPrefix: "/api", AutoMigrate: true})
	server.MustRegister(testutil.User{}, testutil.Post{}, testutil.Comment{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("auto-migrate: %v", err)
	}

	if cfg.Auth == nil {
		cfg.AllowUnauthenticated = true
	}
	return admin.Mount(server, cfg), server
}

// get issues a GET against the panel handler.
func adminGET(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// post issues a CSRF-protected urlencoded POST against the panel handler. The
// double-submit token is supplied identically as cookie and form field.
func adminPOST(t *testing.T, h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	const token = "0123456789abcdef0123456789abcdef"
	form.Set("_csrf", token)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "maniflex_admin_csrf", Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// seedUser creates one user directly through the API and returns its id.
func seedUser(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	rec := adminPOST(t, h, "/admin/users", url.Values{
		"name":     {name},
		"email":    {name + "@example.com"},
		"password": {"secret"},
		"role":     {"editor"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("seed user %q: status %d, body %s", name, rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	return loc[strings.LastIndex(loc, "/")+1:]
}

func TestAdminDashboardCounts(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})
	seedUser(t, h, "alice")

	rec := adminGET(t, h, "/admin/")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Users") || !strings.Contains(body, "Posts") {
		t.Errorf("dashboard missing model cards:\n%s", body)
	}
}

func TestAdminCreateThroughForm(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})

	// The empty create form renders.
	rec := adminGET(t, h, "/admin/users/new")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="email"`) {
		t.Fatalf("create form not rendered: status %d\n%s", rec.Code, rec.Body)
	}

	// Submitting it creates the record and redirects to its detail page.
	id := seedUser(t, h, "bob")

	rec = adminGET(t, h, "/admin/users/"+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bob@example.com") {
		t.Errorf("detail missing created data:\n%s", rec.Body)
	}
}

func TestAdminValidationErrorsReRenderForm(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})

	// Missing the required name/email/password fields.
	rec := adminPOST(t, h, "/admin/users", url.Values{"role": {"editor"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected form re-render (200), got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "field-error") {
		t.Errorf("expected inline field errors:\n%s", body)
	}
}

func TestAdminEditAndUpdate(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})
	id := seedUser(t, h, "carol")

	rec := adminGET(t, h, "/admin/users/"+id+"/edit")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "carol") {
		t.Fatalf("edit form not pre-filled: status %d\n%s", rec.Code, rec.Body)
	}

	rec = adminPOST(t, h, "/admin/users/"+id, url.Values{
		"name": {"Carol Renamed"},
		"role": {"admin"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update status %d, body %s", rec.Code, rec.Body)
	}

	rec = adminGET(t, h, "/admin/users/"+id)
	if !strings.Contains(rec.Body.String(), "Carol Renamed") {
		t.Errorf("update not applied:\n%s", rec.Body)
	}
}

func TestAdminDelete(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})
	id := seedUser(t, h, "dave")

	rec := adminPOST(t, h, "/admin/users/"+id+"/delete", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status %d", rec.Code)
	}
	rec = adminGET(t, h, "/admin/users/"+id)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleted record still resolves: status %d", rec.Code)
	}
}

func TestAdminCSRFRejected(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})

	// No CSRF cookie/field at all.
	req := httptest.NewRequest(http.MethodPost, "/admin/users",
		strings.NewReader("name=x&email=x@x.com&password=p"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", rec.Code)
	}
}

func TestAdminRelationSelectAndFilter(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})
	userID := seedUser(t, h, "erin")

	// The Post create form should offer a FK <select> populated with users.
	rec := adminGET(t, h, "/admin/posts/new")
	if rec.Code != http.StatusOK {
		t.Fatalf("post form status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), userID) {
		t.Errorf("post form FK select missing user option %s:\n%s", userID, rec.Body)
	}

	// Create a post tied to that user.
	rec = adminPOST(t, h, "/admin/posts", url.Values{
		"title":   {"Hello"},
		"body":    {"World"},
		"status":  {"draft"},
		"user_id": {userID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("post create status %d, body %s", rec.Code, rec.Body)
	}

	// Filtering the list by status should keep the matching row.
	rec = adminGET(t, h, "/admin/posts?f_status=draft")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hello") {
		t.Errorf("status filter dropped matching row:\n%s", rec.Body)
	}
	// Filtering by a non-matching status should drop it.
	rec = adminGET(t, h, "/admin/posts?f_status=archived")
	if strings.Contains(rec.Body.String(), ">Hello<") {
		t.Errorf("status filter kept non-matching row:\n%s", rec.Body)
	}
}

func TestAdminReadOnlyHidesMutations(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{ReadOnly: true})

	if rec := adminGET(t, h, "/admin/users/new"); rec.Code == http.StatusOK {
		t.Errorf("read-only panel served the create form")
	}
	rec := adminPOST(t, h, "/admin/users", url.Values{
		"name": {"x"}, "email": {"x@x.com"}, "password": {"p"},
	})
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("read-only panel accepted a create POST: status %d", rec.Code)
	}
}

func TestAdminStaticAssetServed(t *testing.T) {
	h, _ := adminPanel(t, admin.Config{})
	rec := adminGET(t, h, "/admin/static/admin.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("static asset status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), ".btn") {
		t.Errorf("static admin.css not served correctly")
	}
}
