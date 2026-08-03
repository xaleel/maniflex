package e2e_test

// WR-1 — a record handed back from a write must report what the row holds.
//
// A *string / *time.Time column written through the adapter came back nil in the
// returned struct even though the row was correct, because the map→struct bridge
// only assigns on an exact type match and a time.Time is not assignable to a
// *time.Time. The value rode the extra carrier instead, which the generated CRUD
// path overlays back on — so the HTTP response was always right and only
// hand-written code saw the nil. The workaround was to refetch after every write.
//
//	go test ./e2e/ -run TestWriteEcho

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type EchoDoc struct {
	maniflex.BaseModel
	Title       string     `json:"title"        db:"title"`
	Note        *string    `json:"note"         db:"note"`
	PublishedAt *time.Time `json:"published_at" db:"published_at"`
	Rank        *int       `json:"rank"         db:"rank"`
}

func echoServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: []any{EchoDoc{}}})
}

func echoMeta(t *testing.T, srv *testutil.Server) *maniflex.ModelMeta {
	t.Helper()
	meta, ok := srv.ManiflexServer().Registry().Get("EchoDoc")
	if !ok {
		t.Fatal("EchoDoc is not registered")
	}
	return meta
}

func ptr[T any](v T) *T { return &v }

// The symptom, at the layer the audit found it.
func TestWriteEcho_AdapterCreateReportsPointerColumns(t *testing.T) {
	t.Parallel()
	srv := echoServer(t)
	meta := echoMeta(t, srv)
	ctx := context.Background()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	rec, err := srv.ManiflexServer().DB().Create(ctx, meta, &EchoDoc{
		Title: "t", Note: ptr("hello"), PublishedAt: &when, Rank: ptr(7),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := rec.(*EchoDoc)

	if got.Note == nil {
		t.Error("Note came back nil — the row holds it, the echo does not")
	} else if *got.Note != "hello" {
		t.Errorf("Note = %q, want %q", *got.Note, "hello")
	}
	if got.PublishedAt == nil {
		t.Error("PublishedAt came back nil")
	} else if !got.PublishedAt.Equal(when) {
		t.Errorf("PublishedAt = %v, want %v", got.PublishedAt, when)
	}
	if got.Rank == nil || *got.Rank != 7 {
		t.Errorf("Rank = %v, want a pointer to 7", got.Rank)
	}
}

// The echo has to agree with the row, which is the property the refetch
// workaround was buying. Comparing the two is stricter than asserting values:
// it fails if either side drifts.
func TestWriteEcho_MatchesTheRefetch(t *testing.T) {
	t.Parallel()
	srv := echoServer(t)
	meta := echoMeta(t, srv)
	ctx := context.Background()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	rec, err := srv.ManiflexServer().DB().Create(ctx, meta, &EchoDoc{
		Title: "t", Note: ptr("hello"), PublishedAt: &when,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	echo := rec.(*EchoDoc)

	back, err := srv.ManiflexServer().DB().FindByID(ctx, meta, echo.ID, &maniflex.QueryParams{})
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	row := back.(*EchoDoc)

	if !samePtr(echo.Note, row.Note) {
		t.Errorf("Note: echo=%v refetch=%v", deref(echo.Note), deref(row.Note))
	}
	if (echo.PublishedAt == nil) != (row.PublishedAt == nil) {
		t.Errorf("PublishedAt: echo=%v refetch=%v", echo.PublishedAt, row.PublishedAt)
	}
}

// An update's echo is built the same way and had the same hole.
func TestWriteEcho_AdapterUpdateReportsPointerColumns(t *testing.T) {
	t.Parallel()
	srv := echoServer(t)
	meta := echoMeta(t, srv)
	ctx := context.Background()

	rec, err := srv.ManiflexServer().DB().Create(ctx, meta, &EchoDoc{Title: "t"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := rec.(*EchoDoc).ID

	up, err := srv.ManiflexServer().DB().Update(ctx, meta, id,
		map[string]any{"note": "patched"}, map[string]struct{}{"note": {}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := up.(*EchoDoc)
	if got.Note == nil || *got.Note != "patched" {
		t.Errorf("Note = %v, want a pointer to %q", deref(got.Note), "patched")
	}
}

// A column the write did not touch must stay nil rather than being invented,
// and a column written as NULL must read as nil — "no value" is not "the empty
// string".
func TestWriteEcho_AbsentAndNullStayNil(t *testing.T) {
	t.Parallel()
	srv := echoServer(t)
	meta := echoMeta(t, srv)
	ctx := context.Background()

	rec, err := srv.ManiflexServer().DB().Create(ctx, meta, &EchoDoc{Title: "only a title"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := rec.(*EchoDoc)
	if got.Note != nil {
		t.Errorf("Note = %q, want nil — nothing was written to it", *got.Note)
	}
	if got.PublishedAt != nil {
		t.Errorf("PublishedAt = %v, want nil", got.PublishedAt)
	}
	if got.Rank != nil {
		t.Errorf("Rank = %d, want nil", *got.Rank)
	}
}

// The path that never showed the bug must keep not showing it. If the carrier
// stopped winning over the struct field, a *time.Time would start serialising as
// a pointer's target through a different code path and the wire format could
// shift.
func TestWriteEcho_HTTPResponseIsUnchanged(t *testing.T) {
	t.Parallel()
	srv := echoServer(t)

	resp := srv.POST("/echo_docs", map[string]any{
		"title": "t", "note": "hi", "published_at": "2026-01-02T03:04:05Z", "rank": 7,
	})
	resp.AssertStatus(http.StatusCreated)

	data := resp.Data()
	if data["note"] != "hi" {
		t.Errorf("note = %#v, want \"hi\"", data["note"])
	}
	if data["published_at"] != "2026-01-02T03:04:05Z" {
		t.Errorf("published_at = %#v, want the RFC3339 string the map path emits", data["published_at"])
	}
	if n, ok := data["rank"].(float64); !ok || n != 7 {
		t.Errorf("rank = %#v, want 7", data["rank"])
	}
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
