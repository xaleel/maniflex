package e2e

// §10.4: mfx:"index" makes AutoMigrate create a DB index on the column. The
// meta-level wiring (an IndexSpec is appended) is unit-tested in the core
// package; here we assert the index physically lands in the database after
// AutoMigrate by introspecting sqlite_master.

import (
	"context"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type indexedDoc struct {
	maniflex.BaseModel
	Slug  string `json:"slug"  mfx:"index"`
	Title string `json:"title"`
}

func TestIndexTag_E2E_CreatesIndexInDB(t *testing.T) {
	skipRawSQLOnPostgres(t) // sqlite_master introspection is SQLite-specific
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{Models: []any{indexedDoc{}}})

	// AutoMigrate ran during setup; the model boots and serves.
	srv.GET("/indexed_docs").AssertStatus(http.StatusOK)

	bg := maniflex.NewBackground(context.Background(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())

	rows, err := bg.RawQuery(
		"SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='indexed_docs'")
	if err != nil {
		t.Fatalf("introspect indexes: %v", err)
	}

	want := "idx_indexed_docs_slug"
	found := false
	for _, r := range rows {
		if name, _ := r["name"].(string); name == want {
			found = true
		}
	}
	if !found {
		t.Errorf("index %q not created on table indexed_docs; got indexes %v", want, rows)
	}
}
