package e2e_test

// 11D.1 — a composite UNIQUE violation must name every column it involves.
//
// extractColumn returned the first column by design, so a violation of
// UNIQUE(phone_number, owner_id) reported `field: "phone_number"` — blaming one
// column for a constraint that neither column violates on its own. A form
// highlights the wrong input, and the message asserts something false: the phone
// number is not "already taken", the pair is.
//
// Also pinned here: the column must not carry the driver's extended result code.
// SQLite appends one to its message ("email (2067)"), and the parser used to
// hand it through.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
)

// CompContact is unique on the pair, not on either column alone.
type CompContact struct {
	maniflex.BaseModel
	PhoneNumber string `json:"phone_number" db:"phone_number"`
	OwnerID     string `json:"owner_id"     db:"owner_id"`
	Label       string `json:"label"        db:"label"`
}

func compSrv(t *testing.T) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(CompContact{}, maniflex.ModelConfig{
		Indices: []maniflex.IndexSpec{{
			Name:    "idx_comp_contacts_phone_owner",
			Columns: []string{"phone_number", "owner_id"},
			Unique:  true,
		}},
	})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func compPost(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/api/comp_contacts", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return resp.StatusCode, env
}

func compDetails(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object: %v", env)
	}
	raw, ok := errObj["details"].([]any)
	if !ok {
		t.Fatalf("details is %T, want array: %v", errObj["details"], errObj["details"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, d := range raw {
		m, _ := d.(map[string]any)
		out = append(out, m)
	}
	return out
}

func TestCompositeUnique_NamesEveryColumn(t *testing.T) {
	base := compSrv(t)
	const row = `{"phone_number":"555-0100","owner_id":"acme","label":"work"}`

	if code, env := compPost(t, base, row); code != http.StatusCreated {
		t.Fatalf("setup: %d %v", code, env)
	}
	code, env := compPost(t, base, row)
	if code != http.StatusConflict {
		t.Fatalf("duplicate pair: got %d, want 409 — %v", code, env)
	}

	details := compDetails(t, env)
	fields := make(map[string]bool, len(details))
	for _, d := range details {
		f, _ := d["field"].(string)
		fields[f] = true
	}
	for _, want := range []string{"phone_number", "owner_id"} {
		if !fields[want] {
			t.Errorf("details does not name %q — a composite violation blamed on "+
				"one column highlights the wrong input; got %v", want, fields)
		}
	}
}

// TestCompositeUnique_MessageDescribesThePair: naming both columns is only half
// of it. "phone_number already taken" is false — that number is fine, the
// combination is not — so the message must not claim otherwise.
func TestCompositeUnique_MessageDescribesThePair(t *testing.T) {
	base := compSrv(t)
	const row = `{"phone_number":"555-0100","owner_id":"acme","label":"work"}`
	compPost(t, base, row)
	_, env := compPost(t, base, row)

	for _, d := range compDetails(t, env) {
		msg, _ := d["message"].(string)
		if !strings.Contains(msg, "phone_number") || !strings.Contains(msg, "owner_id") {
			t.Errorf("message should name the whole combination, got %q", msg)
		}
	}
}

// TestCompositeUnique_SingleColumnUnchanged is the anti-over-reach pair: the
// common case must keep exactly one detail and its original wording.
func TestCompositeUnique_SingleColumnUnchanged(t *testing.T) {
	base := dupSrv(t) // DupUser, a plain mfx:"unique" column
	if code, _ := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`); code != http.StatusCreated {
		t.Fatal("setup")
	}
	code, env := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)
	if code != http.StatusConflict {
		t.Fatalf("got %d, want 409", code)
	}
	details := compDetails(t, env)
	if len(details) != 1 {
		t.Fatalf("single-column violation produced %d details, want 1: %v", len(details), details)
	}
	if got, _ := details[0]["field"].(string); got != "email" {
		t.Errorf("field = %q, want email", got)
	}
	if got, _ := details[0]["message"].(string); got != "email already taken" {
		t.Errorf("message = %q, want %q", got, "email already taken")
	}
}
