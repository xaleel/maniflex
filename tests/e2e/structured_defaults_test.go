package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/money"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// DefaultItem has two structured SQLTyper columns with no scalar zero default.
type DefaultItem struct {
	maniflex.BaseModel
	Name  string                `json:"name" mfx:"filterable"`
	Label maniflex.LocaleString `json:"label"`
	Price money.Amount          `json:"price"`
}

// Omitting JSON / SQLTyper columns on create must succeed — the migrator now
// gives them an empty-container default — instead of failing with a NOT NULL
// violation (which previously surfaced as an opaque 500). Regression D2-5 / D1-1.
func TestCreate_OmittedStructuredColumns_DefaultEmpty(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{DefaultItem{}}})

	created := srv.POST("/default_items", map[string]any{"name": "Widget"})
	created.AssertStatus(http.StatusCreated)
	id, _ := created.Data()["id"].(string)
	if id == "" {
		t.Fatalf("missing id: %v", created.Data())
	}

	got := srv.GET("/default_items/" + id).Data()
	label, ok := got["label"].(map[string]any)
	if !ok {
		t.Fatalf("omitted LocaleString should default to an empty object, got %T %v", got["label"], got["label"])
	}
	if len(label) != 0 {
		t.Fatalf("label should be empty, got %v", label)
	}
}

// rawText is a SQLTyper with no driver.Valuer and a non-container kind, so the
// migrator cannot synthesise a zero default for it — the column is NOT NULL with
// no DEFAULT. Omitting it on create therefore reaches a genuine DB NOT NULL
// violation.
type rawText struct{ V string }

func (rawText) SQLType(maniflex.DriverType) string { return "TEXT" }

type RequiredBlob struct {
	maniflex.BaseModel
	Name string  `json:"name" mfx:"filterable"`
	Blob rawText `json:"blob"`
}

// A NOT NULL violation that reaches the database must be reported as a 422
// VALIDATION_ERROR (missing required value), not a 409 conflict or an opaque
// 500. Regression for the unmapped-NOT-NULL gap (D1-1 / P1-F).
func TestCreate_NotNullViolation_Returns422(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{RequiredBlob{}}})

	resp := srv.POST("/required_blobs", map[string]any{"name": "Widget"})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("create omitting NOT NULL column: status %d, want 422\nbody: %s", resp.Status, resp.Body)
	}
	if code := resp.ErrorCode(); code != "VALIDATION_ERROR" {
		t.Fatalf("error code %q, want VALIDATION_ERROR\nbody: %s", code, resp.Body)
	}
}
