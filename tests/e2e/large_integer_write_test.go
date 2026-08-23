package e2e

import (
	"regexp"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// bigIntRow carries an int64 past 2^53, where float64 stops holding every
// integer.
type bigIntRow struct {
	maniflex.BaseModel
	N int64 `json:"n" mfx:"filterable"`
}

// nInBody reads the "n" value out of a response as raw text. Decoding the body
// in the test would round it a second time through float64 and hide whatever
// the server actually stored, which is the entire question here.
var nInBody = regexp.MustCompile(`"n":\s*([0-9-]+)`)

func assertN(t *testing.T, label string, body []byte, want string) {
	t.Helper()
	m := nInBody.FindSubmatch(body)
	if m == nil {
		t.Fatalf("%s: no \"n\" in response: %s", label, body)
	}
	if got := string(m[1]); got != want {
		t.Errorf("%s: n = %s, want %s", label, got, want)
	}
}

// A JSON number decoded into a map[string]any is a float64, which represents
// every integer only up to 2^53. Writes do not travel that way: the DB step
// sources columns from the typed record whenever it faithfully covers the body
// (recordSourcedWrite), and encoding/json decodes a JSON number straight into an
// int64 field — exactly. ParsedBody is the fallback, and it is close to
// unreachable for a typed write: RequestBody's backing map is unexported and
// only SetField/DeleteField can mutate it, both of which sync the record, and
// registration requires an embedded BaseModel so the present-set is never nil.
//
// This test pins that arrangement rather than describing it. If the write path
// ever sources from ParsedBody instead, an int64 column silently starts rounding
// above 2^53 and nothing else here would notice: the stored value is still a
// perfectly good number, just not the one that was sent (audit N1).
func TestLargeIntegerSurvivesWriteRoundTrip(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{bigIntRow{}}})

	// 2^53 + 1. float64 rounds this DOWN to 2^53.
	const created = "9007199254740993"
	resp := srv.POST("/big_int_rows", []byte(`{"n": `+created+`}`))
	resp.AssertStatus(201)
	assertN(t, "create echo", resp.Body, created)

	id := srv.MustID(resp)
	assertN(t, "read back from the database", srv.GET("/big_int_rows/"+id).Body, created)

	// MaxInt64, which float64 rounds UP to a value above MaxInt64 — the other
	// rounding direction, and the one that cannot even be stored in the column.
	const updated = "9223372036854775807"
	srv.PATCH("/big_int_rows/"+id, []byte(`{"n": `+updated+`}`)).AssertStatus(200)
	assertN(t, "read back after update", srv.GET("/big_int_rows/"+id).Body, updated)

	// The filter path parses its value from the URL rather than from a JSON
	// body, so it never meets the float64 map at all; asserted so a change that
	// routed filters through the body decoder would show up here.
	assertN(t, "matched by filter", srv.GET("/big_int_rows?filter=n:eq:"+updated).Body, updated)
}
