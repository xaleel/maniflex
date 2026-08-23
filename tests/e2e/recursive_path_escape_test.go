package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// The recursive CTE tracks visited ids as a '/'-delimited string and asks
// "is '/id/' already in the path?" to stop a cyclic parent chain. That test is
// only sound while '/' cannot appear inside an id — and it can. Server-supplied
// ids are a documented feature, and identity.md names "a record keyed by an
// external system's identifier" as a use for them; external identifiers contain
// '/' routinely.
//
// With one in play the delimiter is ambiguous, and a node whose id matches a
// tail segment of an ancestor's id is pruned as already-visited. A real subtree
// disappears from the tree with no error and nothing logged (audit O4).

// seedSlashTree creates a chain root -> mid -> leaf with caller-chosen ids,
// which HTTP cannot do: no route accepts an id, by design. The background
// accessor is the supported way in.
func seedSlashTree(t *testing.T, srv *testutil.Server, rootID, midID, leafID string) {
	t.Helper()
	mfx := srv.ManiflexServer()
	bg := maniflex.NewBackground(t.Context(), mfx.DB(), mfx.Registry())
	for _, row := range []map[string]any{
		{"id": rootID, "name": "Root", "status": "active"},
		{"id": midID, "name": "Mid", "parent_id": rootID, "status": "active"},
		{"id": leafID, "name": "Leaf", "parent_id": midID, "status": "active"},
	} {
		if _, err := bg.GetModel("category").Create(row); err != nil {
			t.Fatalf("seed %v: %v", row["id"], err)
		}
	}
}

func treeIDs(t *testing.T, srv *testutil.Server, rootID string) []string {
	t.Helper()
	rows := srv.GET("/categories/" + rootID + "/tree").AssertStatus(http.StatusOK).DataList()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.(map[string]any)["id"].(string))
	}
	return out
}

// The leaf's id ("b") is a tail segment of its parent's id ("a/b"), so the
// path "/x/a/b/" contains "/b/" and the leaf was dropped as a repeat visit.
func TestRecursiveQuery_SlashInIDDoesNotPruneSubtree(t *testing.T) {
	t.Parallel()
	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})
	seedSlashTree(t, srv, "x", "a/b", "b")

	got := treeIDs(t, srv, "x")
	if len(got) != 3 {
		t.Fatalf("tree returned %d rows (%v), want 3 — the subtree under %q was pruned",
			len(got), got, "a/b")
	}
	for _, want := range []string{"x", "a/b", "b"} {
		if !containsID(got, want) {
			t.Errorf("id %q missing from the tree: %v", want, got)
		}
	}
}

// Escaping '/' is not enough on its own: whatever stands in for it has to be
// escaped too, or two different ids encode to the same segment and the
// collision prunes exactly as the raw '/' did. Here "a/b" and "a~1b" are
// distinct ids that collide under a single-substitution scheme.
func TestRecursiveQuery_EscapeCharacterInIDDoesNotPruneSubtree(t *testing.T) {
	t.Parallel()
	srv := categoryServer(t, func(id string) maniflex.RecursiveQuery {
		return maniflex.RecursiveQuery{RootID: id, ParentField: "parent_id"}
	})
	seedSlashTree(t, srv, "r", "a/b", "a~1b")

	got := treeIDs(t, srv, "r")
	if len(got) != 3 {
		t.Fatalf("tree returned %d rows (%v), want 3 — %q collided with the encoding of %q",
			len(got), got, "a~1b", "a/b")
	}
	for _, want := range []string{"r", "a/b", "a~1b"} {
		if !containsID(got, want) {
			t.Errorf("id %q missing from the tree: %v", want, got)
		}
	}
}

func containsID(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
