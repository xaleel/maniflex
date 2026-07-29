package e2e

// End-to-end coverage for keyset (cursor) pagination — roadmap 4.8.
//
//	go test ./tests/e2e/... -run TestCursorPagination

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// CursorEvent opts into keyset pagination via the cursor_field tag. Seq is a
// monotonic integer so the test can assert an exact walk order; id is the
// implicit tiebreaker.
type CursorEvent struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required,filterable"`
	Seq  int    `json:"seq"  db:"seq"  mfx:"required,filterable,sortable,cursor_field:seq"`
}

// CursorByTime opts in via a cursor_field tag on the embedded BaseModel, the
// canonical created_at case.
type CursorByTime struct {
	maniflex.BaseModel `mfx:"cursor_field:created_at"`
	Name               string `json:"name" db:"name" mfx:"required"`
}

// CursorAtTime exposes a caller-supplied timestamp so the regression can pin
// whole-second, fractional, and tied values deterministically on both database
// lanes.
type CursorAtTime struct {
	maniflex.BaseModel
	Name    string    `json:"name" db:"name" mfx:"required"`
	EventAt time.Time `json:"event_at" db:"event_at" mfx:"required,sortable,cursor_field:event_at"`
}

func cursorServer(t *testing.T) *testutil.Server {
	t.Helper()
	// Register a plain model too so the "not enabled" path can be probed.
	return testutil.NewServer(t, testutil.Options{
		Models: []any{CursorEvent{}, CursorByTime{}, CursorAtTime{}, testutil.User{}},
	})
}

func seedEvents(t *testing.T, srv *testutil.Server, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		resp := srv.POST("/cursor_events", map[string]any{
			"name": "e", "seq": i,
		})
		resp.AssertStatus(http.StatusCreated)
	}
}

// seqList extracts the seq values from a list response in order.
func seqList(t *testing.T, resp *testutil.Response) []int {
	t.Helper()
	var out []int
	for _, it := range resp.DataList() {
		m := it.(map[string]any)
		out = append(out, int(m["seq"].(float64)))
	}
	return out
}

func TestCursorPagination(t *testing.T) {
	t.Parallel()

	t.Run("walks_pages_in_order_with_cursor_meta", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 5)

		// First page — empty cursor value means "from the start".
		p1 := srv.GET("/cursor_events?cursor=&limit=2")
		p1.AssertStatus(http.StatusOK)
		if got := seqList(t, p1); !equalInts(got, []int{1, 2}) {
			t.Fatalf("page 1 seq = %v, want [1 2]", got)
		}
		meta := p1.Meta()
		if meta["has_more"] != true {
			t.Errorf("page 1 has_more = %v, want true", meta["has_more"])
		}
		// Cursor responses must not carry offset fields.
		for _, k := range []string{"total", "page", "pages"} {
			if _, ok := meta[k]; ok {
				t.Errorf("cursor meta should not contain %q: %v", k, meta)
			}
		}
		next, _ := meta["next_cursor"].(string)
		testutil.AssertNotEmpty(t, "next_cursor", next)

		// Second page.
		p2 := srv.GET("/cursor_events?cursor=" + next + "&limit=2")
		p2.AssertStatus(http.StatusOK)
		if got := seqList(t, p2); !equalInts(got, []int{3, 4}) {
			t.Fatalf("page 2 seq = %v, want [3 4]", got)
		}
		next2, _ := p2.Meta()["next_cursor"].(string)
		testutil.AssertNotEmpty(t, "next_cursor 2", next2)

		// Final page — one row left, no more after it.
		p3 := srv.GET("/cursor_events?cursor=" + next2 + "&limit=2")
		p3.AssertStatus(http.StatusOK)
		if got := seqList(t, p3); !equalInts(got, []int{5}) {
			t.Fatalf("page 3 seq = %v, want [5]", got)
		}
		m3 := p3.Meta()
		if m3["has_more"] != false {
			t.Errorf("page 3 has_more = %v, want false", m3["has_more"])
		}
		if nc, ok := m3["next_cursor"]; ok && nc != "" {
			t.Errorf("page 3 next_cursor should be empty, got %v", nc)
		}
	})

	t.Run("no_skip_when_earlier_row_deleted_between_pages", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 5)

		p1 := srv.GET("/cursor_events?cursor=&limit=2") // seq 1,2
		p1.AssertStatus(http.StatusOK)
		next := p1.Meta()["next_cursor"].(string)

		// Delete a row *before* the cursor window. Offset pagination would now
		// skip seq 3; keyset pagination must still return it.
		id1 := p1.DataList()[0].(map[string]any)["id"].(string)
		srv.DELETE("/cursor_events/" + id1).AssertStatus(http.StatusNoContent)

		p2 := srv.GET("/cursor_events?cursor=" + next + "&limit=2")
		p2.AssertStatus(http.StatusOK)
		if got := seqList(t, p2); !equalInts(got, []int{3, 4}) {
			t.Fatalf("page 2 after delete = %v, want [3 4] (no skip)", got)
		}
	})

	t.Run("descending_walk_via_sort", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 5)

		p1 := srv.GET("/cursor_events?cursor=&sort=seq:desc&limit=2")
		p1.AssertStatus(http.StatusOK)
		if got := seqList(t, p1); !equalInts(got, []int{5, 4}) {
			t.Fatalf("desc page 1 seq = %v, want [5 4]", got)
		}
		next := p1.Meta()["next_cursor"].(string)
		p2 := srv.GET("/cursor_events?cursor=" + next + "&sort=seq:desc&limit=2")
		p2.AssertStatus(http.StatusOK)
		if got := seqList(t, p2); !equalInts(got, []int{3, 2}) {
			t.Fatalf("desc page 2 seq = %v, want [3 2]", got)
		}
	})

	t.Run("created_at_cursor_field_on_embedded_base_walks_all_rows_once", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		for i := 0; i < 5; i++ {
			srv.POST("/cursor_by_times", map[string]any{"name": "n"}).AssertStatus(http.StatusCreated)
		}

		// Walk every page and collect ids; the union must be the 5 distinct rows
		// with no skip and no duplicate, even when created_at values tie.
		seen := map[string]bool{}
		cursor := ""
		for pages := 0; pages < 10; pages++ {
			resp := srv.GET("/cursor_by_times?cursor=" + cursor + "&limit=2")
			resp.AssertStatus(http.StatusOK)
			for _, it := range resp.DataList() {
				id := it.(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatalf("row %s returned twice across pages", id)
				}
				seen[id] = true
			}
			m := resp.Meta()
			if m["has_more"] != true {
				break
			}
			cursor = m["next_cursor"].(string)
		}
		if len(seen) != 5 {
			t.Fatalf("walked %d distinct rows, want 5", len(seen))
		}
	})

	t.Run("time_cursor_precision_ties_directions_and_projection", func(t *testing.T) {
		t.Parallel()

		base := time.Date(2026, 7, 28, 12, 34, 56, 0, time.UTC)
		stamps := map[string]time.Time{
			"whole-a": base,
			"whole-b": base,
			"micro":   base.Add(time.Microsecond),
			"half":    base.Add(500 * time.Millisecond),
			"next":    base.Add(time.Second),
		}

		for _, direction := range []struct {
			name string
			sort string
			desc bool
		}{
			{name: "ascending"},
			{name: "descending", sort: "&sort=event_at:desc", desc: true},
		} {
			t.Run(direction.name, func(t *testing.T) {
				srv := cursorServer(t)
				for name, stamp := range stamps {
					srv.POST("/cursor_at_times", map[string]any{
						"name":     name,
						"event_at": stamp.Format(time.RFC3339Nano),
					}).AssertStatus(http.StatusCreated)
				}

				var walked []string
				cursor := ""
				for pages := 0; pages < 10; pages++ {
					// Selecting only name exercises the projection path: query
					// parsing must retain event_at and id internally so it can
					// build the next token.
					resp := srv.GET("/cursor_at_times?cursor=" + cursor +
						direction.sort + "&limit=2&select=name")
					resp.AssertStatus(http.StatusOK)
					for _, item := range resp.DataList() {
						walked = append(walked, item.(map[string]any)["name"].(string))
					}
					if resp.Meta()["has_more"] != true {
						break
					}
					cursor = resp.Meta()["next_cursor"].(string)
				}

				if len(walked) != len(stamps) {
					t.Fatalf("walked %d rows (%v), want %d", len(walked), walked, len(stamps))
				}
				seen := make(map[string]bool, len(walked))
				for i, name := range walked {
					if seen[name] {
						t.Fatalf("row %q returned twice: %v", name, walked)
					}
					seen[name] = true
					if i == 0 {
						continue
					}
					previous, current := stamps[walked[i-1]], stamps[name]
					if (!direction.desc && current.Before(previous)) ||
						(direction.desc && current.After(previous)) {
						t.Fatalf("timestamps out of order at %q -> %q: %v", walked[i-1], name, walked)
					}
				}
			})
		}
	})

	t.Run("invalid_cursor_token_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 1)
		srv.GET("/cursor_events?cursor=%21%21%21not-base64%21%21%21").AssertStatus(http.StatusBadRequest)
	})

	t.Run("invalid_cursor_payloads_return_400_before_database", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 1)

		payloads := map[string]string{
			"missing id":       `{"v":1}`,
			"empty id":         `{"v":1,"id":""}`,
			"null value":       `{"v":null,"id":"row-1"}`,
			"object value":     `{"v":{"nested":true},"id":"row-1"}`,
			"array value":      `{"v":[1,2],"id":"row-1"}`,
			"trailing value":   `{"v":1,"id":"row-1"} true`,
			"wrong field type": `{"version":1,"type":"string","v":"1","id":"row-1"}`,
			"fractional int":   `{"version":1,"type":"integer","v":1.5,"id":"row-1"}`,
			"unbindable uint":  `{"version":1,"type":"integer","v":18446744073709551615,"id":"row-1"}`,
		}
		for name, payload := range payloads {
			t.Run(name, func(t *testing.T) {
				token := base64.RawURLEncoding.EncodeToString([]byte(payload))
				resp := srv.GET("/cursor_events?cursor=" + token)
				resp.AssertStatus(http.StatusBadRequest)
				if code := resp.ErrorCode(); code != "INVALID_QUERY" {
					t.Errorf("error code = %q, want INVALID_QUERY", code)
				}
			})
		}
	})

	t.Run("cursor_on_model_without_cursor_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		srv.GET("/users?cursor=").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_non_cursor_field_rejected", func(t *testing.T) {
		t.Parallel()
		srv := cursorServer(t)
		seedEvents(t, srv, 1)
		srv.GET("/cursor_events?cursor=&sort=name:asc").AssertStatus(http.StatusBadRequest)
	})
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
