package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// Widget is a minimal model used to exercise the panel without a database.
type Widget struct {
	maniflex.BaseModel
	Name string `json:"name" mfx:"required,sortable"`
}

func TestPrettify(t *testing.T) {
	cases := map[string]string{
		"users":      "Users",
		"blog_posts": "Blog Posts",
		"user_id":    "User ID",
		"id":         "ID",
	}
	for in, want := range cases {
		if got := prettify(in); got != want {
			t.Errorf("prettify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCellString(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "—"},
		{"", "—"},
		{"hi", "hi"},
		{true, "✓"},
		{false, "✗"},
		{float64(42), "42"},
		{float64(3.5), "3.5"},
	}
	for _, c := range cases {
		if got := cellString(c.in); got != c.want {
			t.Errorf("cellString(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadEmbeddedTemplates(t *testing.T) {
	ts, err := loadTemplates(nil)
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	for _, name := range pageNames {
		if ts.pages[name] == nil {
			t.Errorf("missing composed page %q", name)
		}
	}
}

func TestMountRequiresAuthDecision(t *testing.T) {
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Widget{})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when neither Auth nor AllowUnauthenticated is set")
		}
	}()
	Mount(server, Config{})
}

func TestDashboardRenders(t *testing.T) {
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Widget{})
	h := Mount(server, Config{AllowUnauthenticated: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Dashboard") {
		t.Errorf("dashboard missing heading:\n%s", body)
	}
	if !strings.Contains(body, "/admin/widgets") {
		t.Errorf("dashboard nav missing model link:\n%s", body)
	}
}

func TestUnknownModelIs404(t *testing.T) {
	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
	server.MustRegister(Widget{})
	h := Mount(server, Config{AllowUnauthenticated: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/nonexistent", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status = %d, want 404", rec.Code)
	}
}
