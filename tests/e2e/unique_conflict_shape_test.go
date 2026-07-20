package e2e_test

// 13.5 — one duplicate value, one answer.
//
// A unique violation caught by the database returned 409 CONFLICT with `details`
// as an object; the same violation caught by validate.UniqueField returned 422
// VALIDATION_ERROR with `details` as an array. Same user error, two statuses, two
// codes and two shapes, decided by which mechanism happened to notice first — so
// a client had to implement both to handle one case.
//
// Unified on 409: a duplicate is a conflict with existing state, not malformed
// input. The array is the shape every other error in the framework already uses.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/middleware/validate"
)

// DupUser has a DB-enforced unique column; DupChecked is guarded by the
// middleware instead. Two models so each path can be driven in isolation.
type DupUser struct {
	maniflex.BaseModel
	Email string `json:"email" db:"email" mfx:"unique"`
}
type DupChecked struct {
	maniflex.BaseModel
	Email string `json:"email" db:"email"`
}

func dupSrv(t *testing.T) string {
	t.Helper()
	rawDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	rawDB.SetMaxOpenConns(1)
	t.Cleanup(func() { rawDB.Close() })

	server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	server.MustRegister(DupUser{}, DupChecked{})
	adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
	adapter.SetErrorNormalizer(sqlite.NormalizeError)
	server.SetDB(adapter)
	if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	server.Pipeline.Validate.Register(
		validate.UniqueField(rawDB, maniflex.SQLite, "email"),
		maniflex.ForModel("DupChecked"))

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func dupPost(t *testing.T, base, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/api"+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("body is not JSON: %s", b)
	}
	return resp.StatusCode, env
}

// assertConflictShape is the contract both paths must satisfy.
func assertConflictShape(t *testing.T, who string, code int, env map[string]any) {
	t.Helper()
	if code != http.StatusConflict {
		t.Errorf("%s: status = %d, want 409", who, code)
	}
	errObj, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no error object in %v", who, env)
	}
	if got := errObj["code"]; got != "CONFLICT" {
		t.Errorf("%s: code = %v, want CONFLICT", who, got)
	}
	// The array is the point: every other error in the framework uses one, and a
	// client that ranges over details must not have to type-switch first.
	details, ok := errObj["details"].([]any)
	if !ok {
		t.Fatalf("%s: details is %T, want an array — %v", who, errObj["details"], errObj["details"])
	}
	if len(details) == 0 {
		t.Fatalf("%s: details is empty", who)
	}
	d0, _ := details[0].(map[string]any)
	if d0["field"] != "email" {
		t.Errorf("%s: details[0].field = %v, want email", who, d0["field"])
	}
	if msg, _ := d0["message"].(string); msg == "" {
		t.Errorf("%s: details[0].message is empty", who)
	}
}

func TestUniqueConflict_DBPathShape(t *testing.T) {
	base := dupSrv(t)
	if code, _ := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`); code != http.StatusCreated {
		t.Fatalf("setup create: %d", code)
	}
	code, env := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)
	assertConflictShape(t, "db-constraint", code, env)
}

func TestUniqueConflict_MiddlewarePathShape(t *testing.T) {
	base := dupSrv(t)
	if code, _ := dupPost(t, base, "/dup_checkeds", `{"email":"a@b.c"}`); code != http.StatusCreated {
		t.Fatalf("setup create: %d", code)
	}
	code, env := dupPost(t, base, "/dup_checkeds", `{"email":"a@b.c"}`)
	assertConflictShape(t, "validate.UniqueField", code, env)
}

// TestUniqueConflict_BothPathsAgree is the actual ask: not merely that each is
// well-formed, but that a client cannot tell them apart.
func TestUniqueConflict_BothPathsAgree(t *testing.T) {
	base := dupSrv(t)
	dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)
	dupPost(t, base, "/dup_checkeds", `{"email":"a@b.c"}`)

	dbCode, dbEnv := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)
	mwCode, mwEnv := dupPost(t, base, "/dup_checkeds", `{"email":"a@b.c"}`)

	if dbCode != mwCode {
		t.Errorf("statuses differ: db=%d middleware=%d", dbCode, mwCode)
	}
	dbErr, _ := dbEnv["error"].(map[string]any)
	mwErr, _ := mwEnv["error"].(map[string]any)
	if dbErr["code"] != mwErr["code"] {
		t.Errorf("codes differ: db=%v middleware=%v", dbErr["code"], mwErr["code"])
	}
	if dbErr["message"] != mwErr["message"] {
		t.Errorf("messages differ: db=%q middleware=%q", dbErr["message"], mwErr["message"])
	}
	dbJSON, _ := json.Marshal(dbErr["details"])
	mwJSON, _ := json.Marshal(mwErr["details"])
	if string(dbJSON) != string(mwJSON) {
		t.Errorf("details differ:\n  db         = %s\n  middleware = %s", dbJSON, mwJSON)
	}
}

// TestUniqueConflict_NoDriverNoiseInMessage: SQLite's error string carries an
// extended result code, and the field name was already stripped of it while the
// message was not — so the message read "email (2067) already taken".
func TestUniqueConflict_NoDriverNoiseInMessage(t *testing.T) {
	base := dupSrv(t)
	dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)
	_, env := dupPost(t, base, "/dup_users", `{"email":"a@b.c"}`)

	errObj, _ := env["error"].(map[string]any)
	details, _ := errObj["details"].([]any)
	if len(details) == 0 {
		t.Fatal("no details")
	}
	msg, _ := details[0].(map[string]any)["message"].(string)
	if bytes.ContainsAny([]byte(msg), "()") {
		t.Errorf("message carries driver noise: %q", msg)
	}
}
