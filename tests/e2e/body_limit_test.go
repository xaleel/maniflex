package e2e

// Request-body size limiting. An oversized JSON body must be rejected with
// 413 BODY_TOO_LARGE, never silently truncated to fit — and body.MaxBodySize
// must genuinely move the ceiling it advertises.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/body"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// defaultMaxBody mirrors the framework's built-in 4 MB body ceiling.
const defaultMaxBody = 4 << 20

// bigDoc is a model with one unbounded text column, so a request body's size is
// the only thing under test.
type bigDoc struct {
	maniflex.BaseModel
	Body string `json:"body" db:"body"`
}

func bigDocServer(t *testing.T, mw ...maniflex.MiddlewareFunc) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{bigDoc{}},
		Middleware: func(s *maniflex.Server) {
			for _, m := range mw {
				s.Pipeline.Deserialize.Register(m)
			}
		},
	})
}

// paddedJSON returns a complete JSON object padded with trailing whitespace to
// exactly total bytes. Whitespace is legal JSON padding, so a reader that
// truncates at its limit is left holding a perfectly parseable object — and
// accepts a request on a body the client never finished sending.
func paddedJSON(total int) []byte {
	const obj = `{"body":"padded"}`
	return []byte(obj + strings.Repeat(" ", total-len(obj)))
}

// The truncating read at its worst: the surplus is discarded, the prefix parses
// cleanly, and the record is created from a body the client never sent (BUG-4).
func TestBodyLimit_OversizedBodyRejectedNotTruncated(t *testing.T) {
	t.Parallel()
	srv := bigDocServer(t)

	resp := srv.POST("/big_docs", paddedJSON(defaultMaxBody+1024))
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "BODY_TOO_LARGE" {
		t.Errorf("error code: got %q, want BODY_TOO_LARGE", code)
	}

	// And nothing was written from the truncated prefix.
	if items := srv.GET("/big_docs").DataList(); len(items) != 0 {
		t.Errorf("got %d records, want 0 — a truncated body was accepted", len(items))
	}
}

// The limit is a ceiling, not a target: a body of exactly the limit still passes.
func TestBodyLimit_BodyExactlyAtLimitAccepted(t *testing.T) {
	t.Parallel()
	srv := bigDocServer(t)

	srv.POST("/big_docs", paddedJSON(defaultMaxBody)).AssertStatus(http.StatusCreated)
}

// A lowered ceiling rejects with the 413 the middleware documents, rather than
// surfacing the truncated read as a 400.
func TestBodyLimit_MaxBodySizeLowersTheCeiling(t *testing.T) {
	t.Parallel()
	srv := bigDocServer(t, body.MaxBodySize(1<<10)) // 1 KB

	resp := srv.POST("/big_docs", paddedJSON(4<<10))
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "BODY_TOO_LARGE" {
		t.Errorf("error code: got %q, want BODY_TOO_LARGE", code)
	}
}

// A raised ceiling is actually raised: a 5 MB body goes through intact, where
// the default reader would have stopped at 4 MB mid-string.
func TestBodyLimit_MaxBodySizeRaisesTheCeiling(t *testing.T) {
	t.Parallel()
	srv := bigDocServer(t, body.MaxBodySize(8<<20)) // 8 MB

	const payload = 5 << 20
	raw := []byte(`{"body":"` + strings.Repeat("a", payload) + `"}`)

	resp := srv.POST("/big_docs", raw)
	resp.AssertStatus(http.StatusCreated)

	stored, ok := resp.Data()["body"].(string)
	if !ok {
		t.Fatalf("body field: got %T, want string", resp.Data()["body"])
	}
	if len(stored) != payload {
		t.Errorf("stored body is %d bytes, want %d — the body was truncated", len(stored), payload)
	}
}
