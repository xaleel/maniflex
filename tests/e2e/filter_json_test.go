package e2e

// The bug this closes, end to end against a real database (todo/ASKS.md issue 1).
//
// `contains` was the only operator that looked like it asked whether a JSON array
// held a value, and it compiles to a substring LIKE. On SQLite that matched across
// element boundaries — a filter for "cat-1" answered 200 with a row holding
// ["cat-10"] — and on Postgres the same query could not run at all, because LIKE
// has no JSONB overload. Silently wrong in development, a hard error in production.

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	maniflex "github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// fjArray is the shape a JSON array column has: a named type carrying the SQL
// type per driver, plus Valuer/Scanner.
type fjArray []string

func (fjArray) SQLType(d maniflex.DriverType) string {
	if d == maniflex.Postgres {
		return "JSONB"
	}
	return "TEXT"
}
func (a fjArray) Value() (driver.Value, error) {
	b, err := json.Marshal([]string(a))
	return string(b), err
}
func (a *fjArray) Scan(v any) error { return fjScan(v, (*[]string)(a)) }

type fjObject map[string]any

func (fjObject) SQLType(d maniflex.DriverType) string { return fjArray(nil).SQLType(d) }
func (o fjObject) Value() (driver.Value, error) {
	b, err := json.Marshal(map[string]any(o))
	return string(b), err
}
func (o *fjObject) Scan(v any) error { return fjScan(v, (*map[string]any)(o)) }

func fjScan(v, into any) error {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return json.Unmarshal(t, into)
	case string:
		return json.Unmarshal([]byte(t), into)
	}
	return fmt.Errorf("cannot scan %T", v)
}

type fjMerchant struct {
	maniflex.BaseModel
	Name        string   `json:"name"`
	CategoryIDs fjArray  `json:"categoryIds" mfx:"filterable,json_array"`
	Meta        fjObject `json:"meta"        mfx:"filterable,json_object"`
}

func fjServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: []any{fjMerchant{}}})
}

func TestJSONArrayMembershipFilter(t *testing.T) {
	t.Parallel()
	srv := fjServer(t)

	srv.POST("/fj_merchants", map[string]any{
		"name": "ten", "categoryIds": []string{"cat-10"}, "meta": map[string]any{"tier": "gold"},
	}).AssertStatus(http.StatusCreated)
	srv.POST("/fj_merchants", map[string]any{
		"name": "one", "categoryIds": []string{"cat-1", "urgent"}, "meta": map[string]any{"tier": "silver"},
	}).AssertStatus(http.StatusCreated)

	// The reported case. cat-1 is a prefix of cat-10, and only the row that
	// actually holds it may come back.
	names := fjNames(t, srv, "?filter=categoryIds:has:cat-1")
	if len(names) != 1 || names[0] != "one" {
		t.Errorf("has:cat-1 returned %v, want [one] — cat-10 is a different element", names)
	}

	if names := fjNames(t, srv, "?filter=categoryIds:has:cat-10"); len(names) != 1 || names[0] != "ten" {
		t.Errorf("has:cat-10 returned %v, want [ten]", names)
	}
	if names := fjNames(t, srv, "?filter=categoryIds:has:missing"); len(names) != 0 {
		t.Errorf("has:missing returned %v, want none", names)
	}
	if names := fjNames(t, srv, "?filter=categoryIds:not_has:cat-1"); len(names) != 1 || names[0] != "ten" {
		t.Errorf("not_has:cat-1 returned %v, want [ten]", names)
	}
}

func TestJSONObjectPairFilter(t *testing.T) {
	t.Parallel()
	srv := fjServer(t)

	srv.POST("/fj_merchants", map[string]any{
		"name": "g", "categoryIds": []string{}, "meta": map[string]any{"tier": "gold"},
	}).AssertStatus(http.StatusCreated)
	srv.POST("/fj_merchants", map[string]any{
		"name": "s", "categoryIds": []string{}, "meta": map[string]any{"tier": "silver"},
	}).AssertStatus(http.StatusCreated)

	if names := fjNames(t, srv, "?filter=meta:has:tier=gold"); len(names) != 1 || names[0] != "g" {
		t.Errorf("has:tier=gold returned %v, want [g]", names)
	}
	if names := fjNames(t, srv, "?filter=meta:has:tier=bronze"); len(names) != 0 {
		t.Errorf("has:tier=bronze returned %v, want none", names)
	}
	// A key the document does not hold is a non-match, not an error.
	if names := fjNames(t, srv, "?filter=meta:has:absent=gold"); len(names) != 0 {
		t.Errorf("has on an absent key returned %v, want none", names)
	}
	if names := fjNames(t, srv, "?filter=meta:not_has:tier=gold"); len(names) != 1 || names[0] != "s" {
		t.Errorf("not_has:tier=gold returned %v, want [s]", names)
	}
}

// The old spelling must now say so rather than answer.
func TestSubstringFilterOnJSONColumnIsRefused(t *testing.T) {
	t.Parallel()
	srv := fjServer(t)
	srv.POST("/fj_merchants", map[string]any{
		"name": "ten", "categoryIds": []string{"cat-10"}, "meta": map[string]any{},
	}).AssertStatus(http.StatusCreated)

	r := srv.GET("/fj_merchants?filter=categoryIds:contains:cat-1")
	r.AssertStatus(http.StatusBadRequest)
	if code := r.ErrorCode(); code != "INVALID_QUERY" {
		t.Errorf("error code = %q, want INVALID_QUERY", code)
	}
}

func TestHasOnAPlainColumnIsRefused(t *testing.T) {
	t.Parallel()
	fjServer(t).GET("/fj_merchants?filter=name:has:ten").AssertStatus(http.StatusBadRequest)
}

func fjNames(t *testing.T, srv *testutil.Server, query string) []string {
	t.Helper()
	r := srv.GET("/fj_merchants" + query)
	r.AssertStatus(http.StatusOK)
	var out []string
	for _, row := range r.DataList() {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("row is %T", row)
		}
		out = append(out, fmt.Sprint(m["name"]))
	}
	return out
}
