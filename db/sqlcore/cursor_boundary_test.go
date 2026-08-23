package sqlcore

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// finalizeCursorPage set HasMore and then assigned whatever EncodeCursor
// returned — including "". The response renders next_cursor with omitempty, so
// the client received {"has_more": true} and no token: told there are more rows
// and given no way to ask for them, with nothing logged anywhere (audit O3).
//
// The audit put the trigger at an array- or struct-kind cursor field. That one
// cannot happen: collectCursorField rejects those at registration, along with
// pointer (nullable) fields. What survives registration and still fails to
// encode is a float64 holding NaN or ±Inf — encodeCursorValue refuses those
// explicitly, and a float64 cursor field registers without complaint.
//
// The test is written against the invariant rather than that one value, because
// the encoder's accepted set and the registration validator's accepted set are
// maintained separately and have already drifted apart once: whatever the next
// gap between them is, "has_more with no token" is the state that must not ship.
func TestFinalizeCursorPageRefusesUnencodableBoundary(t *testing.T) {
	unencodable := []struct {
		name  string
		value any
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"nil (a NULL in an out-of-band schema)", nil},
	}

	for _, tc := range unencodable {
		t.Run(tc.name, func(t *testing.T) {
			cur := &maniflex.CursorParams{Field: "score"}
			kept, err := finalizeCursorPage(cur, 3, 2, func(int) (any, string) {
				return tc.value, "row-2"
			})
			if err == nil {
				t.Fatalf("no error; kept=%d HasMore=%v NextCursor=%q",
					kept, cur.HasMore, cur.NextCursor)
			}
			if !strings.Contains(err.Error(), "score") {
				t.Errorf("error does not name the cursor field: %v", err)
			}
			// The state this exists to prevent must not be left behind even
			// though the caller is expected to abandon the page.
			if cur.HasMore && cur.NextCursor == "" {
				t.Error("left HasMore set with no token")
			}
		})
	}
}

// The ordinary path must be untouched: a page that has more rows and a value
// that encodes still reports both.
func TestFinalizeCursorPageKeepsWorkingBoundary(t *testing.T) {
	cur := &maniflex.CursorParams{Field: "score"}
	kept, err := finalizeCursorPage(cur, 3, 2, func(int) (any, string) {
		return 42.5, "row-2"
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kept != 2 {
		t.Errorf("kept = %d, want 2", kept)
	}
	if !cur.HasMore {
		t.Error("HasMore = false, want true")
	}
	if cur.NextCursor == "" {
		t.Error("NextCursor is empty")
	}
	v, id, decErr := maniflex.DecodeCursor(cur.NextCursor)
	if decErr != nil {
		t.Fatalf("the token it issued does not decode: %v", decErr)
	}
	if id != "row-2" {
		t.Errorf("decoded id = %q, want row-2", id)
	}
	if got := fmt.Sprint(v); got != "42.5" {
		t.Errorf("decoded value = %s (%T), want 42.5", got, v)
	}
}

// A page that does not over-fetch reports no further rows and no token, as before.
func TestFinalizeCursorPageLastPageUnchanged(t *testing.T) {
	cur := &maniflex.CursorParams{Field: "score"}
	kept, err := finalizeCursorPage(cur, 2, 2, func(int) (any, string) {
		t.Fatal("rowKey must not be called when the page did not over-fetch")
		return nil, ""
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kept != 2 {
		t.Errorf("kept = %d, want 2", kept)
	}
	if cur.HasMore || cur.NextCursor != "" {
		t.Errorf("last page reported HasMore=%v NextCursor=%q", cur.HasMore, cur.NextCursor)
	}
}
