package e2e

// Audit STEP-5 — the 422 the DB step raises when the database catches a NOT
// NULL violation built its details as a single map[string]string, while every
// other framework error — including the Validate step's own required check,
// three lines of code away in the same response shape — sends an array.
//
// responses.md states the contract flatly: "details is an array wherever it is
// present". A client that ranges over details broke on exactly this branch, and
// v1.0 freezes the wire format.
//
// The same fix resolves the driver's column name back to the JSON name the
// client sent. The column reaches this branch straight from the SQL error, so
// a model with db:"headline_col" json:"headline" reported a name the client had
// never seen and could not map back to its own form field.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// NotNullDoc's required column is the one shape columnDef gives no synthesised
// zero DEFAULT (adapter.go, "don't default-zero required fields"), so it is the
// one that can reach the database missing.
type NotNullDoc struct {
	maniflex.BaseModel
	Headline string `json:"headline" db:"headline_col" mfx:"required"`
	Slug     string `json:"slug"     db:"slug"         mfx:"unique"`
	Note     string `json:"note"     db:"note"`
}

// detailsOf returns error.details as the array the contract promises, failing
// the test with the actual body when it is anything else — which is the
// regression this file exists for.
func detailsOf(t *testing.T, resp *testutil.Response) []map[string]any {
	t.Helper()
	var body struct {
		Error struct {
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("parse body: %v\nbody: %s", err, resp.Body)
	}
	var arr []map[string]any
	if err := json.Unmarshal(body.Error.Details, &arr); err != nil {
		t.Fatalf("error.details is not an array — every maniflex error sends one, "+
			"and responses.md publishes that as the contract: %v\nbody: %s",
			err, resp.Body)
	}
	return arr
}

// strippingSrv removes a required field after the built-in required check has
// passed, which is how a request reaches the database without it. The same
// branch answers a NOT NULL column a hand-written migration added outside the
// model.
func strippingSrv(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{NotNullDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.DeleteField("headline")
				return next()
			}, maniflex.ForModel("NotNullDoc"), maniflex.AtPosition(maniflex.After))
		},
	})
}

func TestNotNullDetails_IsAnArray(t *testing.T) {
	t.Parallel()
	srv := strippingSrv(t)

	resp := srv.POST("/not_null_docs", map[string]any{"headline": "h", "note": "n"})
	resp.AssertStatus(http.StatusUnprocessableEntity)

	details := detailsOf(t, resp)
	if len(details) != 1 {
		t.Fatalf("details has %d entries, want 1: %s", len(details), resp.Body)
	}
	if got := details[0]["message"]; got != `field "headline" is required` {
		t.Errorf("message = %#v, want the wording the Validate step uses", got)
	}
}

// The column the driver reports is a database name. The client sent a JSON one
// and has to be able to match the error back to the field it typed in.
func TestNotNullDetails_NamesTheJSONFieldNotTheColumn(t *testing.T) {
	t.Parallel()
	srv := strippingSrv(t)

	resp := srv.POST("/not_null_docs", map[string]any{"headline": "h"})
	resp.AssertStatus(http.StatusUnprocessableEntity)

	details := detailsOf(t, resp)
	if got := details[0]["field"]; got != "headline" {
		t.Errorf("details[0].field = %#v, want %q — %q is the db: name, which the "+
			"client never sent", got, "headline", "headline_col")
	}
}

// Both routes to a missing required value — the Validate step's own check and
// the database's — must now answer in one shape, which is the whole point of
// the finding. Asserting the two against each other is what stops them drifting
// apart again.
func TestNotNullDetails_MatchesTheValidateStepsOwn422(t *testing.T) {
	t.Parallel()
	plain := testutil.NewServer(t, testutil.Options{Models: []any{NotNullDoc{}}})

	fromValidate := plain.POST("/not_null_docs", map[string]any{"note": "n"})
	fromValidate.AssertStatus(http.StatusUnprocessableEntity)

	stripped := strippingSrv(t)
	fromDB := stripped.POST("/not_null_docs", map[string]any{"headline": "h", "note": "n"})
	fromDB.AssertStatus(http.StatusUnprocessableEntity)

	a, b := detailsOf(t, fromValidate), detailsOf(t, fromDB)
	if a[0]["field"] != b[0]["field"] || a[0]["message"] != b[0]["message"] {
		t.Errorf("the same missing field answers in two shapes:\n  validate: %#v\n  database: %#v",
			a[0], b[0])
	}
}

// notNullOutsideModel forces the branch a hand-written migration produces: a
// NOT NULL column the model does not describe, so there is no JSON name to
// resolve and the raw column is the only honest answer. SQLite will not add
// such a column to an existing table, so the constraint error is injected at
// the seam the DB step actually reads it from.
type notNullOutsideModel struct {
	maniflex.DBAdapter
}

func (a *notNullOutsideModel) Create(ctx context.Context, m *maniflex.ModelMeta, record any) (any, error) {
	return nil, &maniflex.ErrConstraint{
		Kind:    maniflex.ConstraintNotNull,
		Table:   m.TableName,
		Column:  "audit_stamp",
		Columns: []string{"audit_stamp"},
		Detail:  "NOT NULL constraint failed: not_null_docs.audit_stamp",
	}
}

func TestNotNullDetails_ColumnOutsideTheModelKeepsItsName(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{NotNullDoc{}},
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			return &notNullOutsideModel{DBAdapter: inner}, nil
		},
	})

	resp := srv.POST("/not_null_docs", map[string]any{"headline": "h"})
	resp.AssertStatus(http.StatusUnprocessableEntity)

	details := detailsOf(t, resp)
	if got := details[0]["field"]; got != "audit_stamp" {
		t.Errorf("details[0].field = %#v, want the raw column — the model has no "+
			"JSON name for it and inventing one would be worse", got)
	}
}

// The array must not become a blanket rewrite of every constraint answer: the
// unique 409 immediately below this branch keeps the shape it has published
// since v0.3.0, and an ordinary create still succeeds.
func TestNotNullDetails_NeighbouringPathsUnchanged(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{NotNullDoc{}}})

	srv.POST("/not_null_docs", map[string]any{"headline": "h", "slug": "taken"}).
		AssertStatus(http.StatusCreated)

	dup := srv.POST("/not_null_docs", map[string]any{"headline": "h2", "slug": "taken"})
	dup.AssertStatus(http.StatusConflict)
	if got := detailsOf(t, dup)[0]["field"]; got != "slug" {
		t.Errorf("409 details[0].field = %#v, want %q", got, "slug")
	}
}
