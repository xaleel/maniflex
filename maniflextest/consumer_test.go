package maniflextest_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/maniflextest"
)

type Widget struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required,filterable"`
}

func requireTestUser(ctx *maniflex.ServerContext, next func() error) error {
	if ctx.Auth == nil {
		ctx.Abort(http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
		return nil
	}
	if ctx.Auth.UserID != "user-42" {
		ctx.Abort(http.StatusForbidden, "FORBIDDEN", "wrong test user")
		return nil
	}
	return next()
}

// ANCHOR: basic-harness
func TestHarness(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models: []any{Widget{}},
		Setup: func(app *maniflex.Server) {
			app.Pipeline.Auth.Register(requireTestUser)
		},
	})

	server.POST("/widgets", map[string]any{"name": "unauthenticated"}).
		AssertStatus(http.StatusUnauthorized)

	created := server.POST(
		"/widgets",
		map[string]any{"name": "first"},
		maniflextest.As(maniflextest.Human("user-42", "editor")),
	).AssertStatus(http.StatusCreated)

	widget := maniflextest.DecodeData[Widget](created)
	if widget.Name != "first" || widget.ID == "" {
		t.Fatalf("unexpected widget: %+v", widget)
	}
}

// ANCHOR_END: basic-harness

func TestFixturesAndTypedLists(t *testing.T) {
	server := maniflextest.New(t, maniflextest.Options{
		Models: []any{Widget{}},
	})

	fixtures := server.Seed(maniflextest.Factory(
		"widget",
		"/widgets",
		3,
		func(i int) map[string]any {
			return map[string]any{"name": fmt.Sprintf("widget-%d", i)}
		},
	)...)

	if id := fixtures.ID(t, "widget[1]"); id == "" {
		t.Fatal("fixture ID is empty")
	}
	list := maniflextest.DecodeDataList[Widget](
		server.GET("/widgets").AssertStatus(http.StatusOK),
	)
	if len(list) != 3 {
		t.Fatalf("widget count: got %d, want 3", len(list))
	}
}

func TestServersUseIsolatedSQLiteDatabases(t *testing.T) {
	first := maniflextest.New(t, maniflextest.Options{Models: []any{Widget{}}})
	first.POST("/widgets", map[string]any{"name": "only in first"}).
		AssertStatus(http.StatusCreated)

	second := maniflextest.New(t, maniflextest.Options{Models: []any{Widget{}}})
	list := maniflextest.DecodeDataList[Widget](
		second.GET("/widgets").AssertStatus(http.StatusOK),
	)
	if len(list) != 0 {
		t.Fatalf("second server saw %d records from the first server", len(list))
	}

	first.GET("/health").AssertStatus(http.StatusOK)
}
