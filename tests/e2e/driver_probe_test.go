package e2e

import (
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestDriverProbe asserts the server actually connects to the backend the
// active lane selected — so "passes on Postgres" means it genuinely hit
// Postgres, not a silent SQLite fallback.
func TestDriverProbe(t *testing.T) {
	t.Parallel()

	srv := testutil.NewServer(t, testutil.Options{})

	dt, ok := srv.ManiflexServer().DB().(interface {
		DriverType() maniflex.DriverType
	})
	if !ok {
		t.Fatalf("adapter %T does not expose DriverType()", srv.ManiflexServer().DB())
	}

	want := maniflex.SQLite
	if testutil.IsPostgres() {
		want = maniflex.Postgres
	}
	if got := dt.DriverType(); got != want {
		t.Fatalf("active driver = %v, want %v (lane=%s)", got, want, testutil.Driver())
	}
}
