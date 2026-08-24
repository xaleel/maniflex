package e2e

// VL-1 — minlen:/maxlen: over HTTP, and the OpenAPI keywords they produce.

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// tagList is the documented way to persist a []string — a bare slice has no SQL
// column mapping, so it is wrapped in a named SQLTyper.
type tagList []string

func (tagList) SQLType(d maniflex.DriverType) string {
	if d == maniflex.Postgres {
		return "JSONB"
	}
	return "TEXT"
}

func (t tagList) Value() (driver.Value, error) {
	b, err := json.Marshal([]string(t))
	return string(b), err
}

func (t *tagList) Scan(v any) error {
	switch s := v.(type) {
	case nil:
		*t = nil
		return nil
	case []byte:
		return json.Unmarshal(s, (*[]string)(t))
	case string:
		if s == "" {
			*t = nil
			return nil
		}
		return json.Unmarshal([]byte(s), (*[]string)(t))
	}
	return fmt.Errorf("tagList: cannot scan %T", v)
}

// LenAccount is the shape the tutorial wanted: a password with a minimum
// length, which used to be written mfx:"min:8" and rejected every value.
type LenAccount struct {
	maniflex.BaseModel
	Email    string  `json:"email"    db:"email"    mfx:"required"`
	Password string  `json:"password" db:"password" mfx:"required,writeonly,minlen:8"`
	Bio      string  `json:"bio"      db:"bio"      mfx:"maxlen:5"`
	Tags     tagList `json:"tags"     db:"tags"     mfx:"maxlen:2"`
}

func lenServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: []any{LenAccount{}}})
}

func TestFieldLen_ShortValueIsRejected(t *testing.T) {
	t.Parallel()
	srv := lenServer(t)

	resp := srv.POST("/len_accounts", map[string]any{
		"email": "a@b.c", "password": "short",
	})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422\n%s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "password") {
		t.Fatalf("the error must name the field: %s", resp.Body)
	}
	// The old failure mode. If this ever comes back, the tag is numeric again.
	if strings.Contains(string(resp.Body), "must be a number") {
		t.Fatalf("a length bound must not report a numeric error: %s", resp.Body)
	}
}

func TestFieldLen_ValidValueIsAccepted(t *testing.T) {
	t.Parallel()
	srv := lenServer(t)
	srv.POST("/len_accounts", map[string]any{
		"email": "a@b.c", "password": "longenough",
	}).AssertStatus(http.StatusCreated)
}

// Characters, not bytes: a 5-character value must pass a maxlen:5 whatever
// script it is written in.
func TestFieldLen_CountsCharactersNotBytes(t *testing.T) {
	t.Parallel()
	srv := lenServer(t)

	for _, bio := range []string{"hello", "cafés", "مرحبا", "😀😀😀😀😀"} {
		resp := srv.POST("/len_accounts", map[string]any{
			"email": "a@b.c", "password": "longenough", "bio": bio,
		})
		if resp.Status != http.StatusCreated {
			t.Errorf("bio %q is 5 characters and must be accepted: %d %s",
				bio, resp.Status, resp.Body)
		}
	}

	resp := srv.POST("/len_accounts", map[string]any{
		"email": "a@b.c", "password": "longenough", "bio": "abcdef",
	})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Errorf("6 characters must be rejected: %d %s", resp.Status, resp.Body)
	}
}

// On a list the bound counts elements.
func TestFieldLen_BoundsListLength(t *testing.T) {
	t.Parallel()
	srv := lenServer(t)

	srv.POST("/len_accounts", map[string]any{
		"email": "a@b.c", "password": "longenough", "tags": []string{"x", "y"},
	}).AssertStatus(http.StatusCreated)

	resp := srv.POST("/len_accounts", map[string]any{
		"email": "a@b.c", "password": "longenough", "tags": []string{"x", "y", "z"},
	})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("3 items must be rejected by maxlen:2: %d %s", resp.Status, resp.Body)
	}
}

// A declared bound has to reach the spec, or a generated client will not know
// about a rule the server enforces.
func TestFieldLen_AppearsInOpenAPI(t *testing.T) {
	t.Parallel()
	srv := lenServer(t)

	body := string(srv.GET("/openapi.json").Body)
	for _, want := range []string{`"minLength":8`, `"maxLength":5`} {
		if !strings.Contains(body, want) {
			t.Errorf("openapi.json is missing %s", want)
		}
	}

	// The list field's bound used to be unassertable here: a slice-typed column
	// was omitted from the model schema entirely, so `tags` had no property to
	// carry a maxItems. That gap is closed (ASKS.md issue 2), so the bound the
	// server enforces on a list now has to reach the spec like any other.
	if !strings.Contains(body, `"maxItems":2`) {
		t.Errorf("openapi.json does not carry the list bound maxlen:2 as maxItems; "+
			"the server rejects a third element and the spec does not say so:\n%s", body)
	}
}
