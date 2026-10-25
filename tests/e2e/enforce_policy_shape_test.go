package e2e_test

// Audit AUTH-21: one Policy was handed four differently-shaped maps.
//
// create and list were keyed by JSON name, read/update/delete/history by DB
// column; a nullable column arrived as a *string on the second half and a string
// on the first; and mfx:"hidden" / mfx:"writeonly" fields were present on some
// branches and absent on others. On any model whose two names differ — any
// camelCase API over snake_case columns — a policy could only be written
// correctly for half the operations, and the failing half failed *open*:
//
//	r["isArchived"] != true
//
// holds on create and list and is vacuously true on read, update and delete, so
// the rule that freezes archived records froze nothing on the operations it
// existed for.
//
// Nothing caught it because every model in the suite, and every example in the
// docs, used fields whose JSON and DB names coincide.
//
//	go test ./tests/e2e/... -run TestEnforcePolicyShape

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Every name differs between JSON and DB, and the model carries one nullable
// column plus one hidden and one writeonly field.
type chartEntry struct {
	maniflex.BaseModel
	Title      string  `json:"title" db:"title"`
	OwnerID    string  `json:"ownerId" db:"owner_id"`
	OrgID      *string `json:"orgId" db:"org_id"`
	IsArchived bool    `json:"isArchived" db:"is_archived"`
	RiskScore  int     `json:"riskScore" db:"risk_score" mfx:"hidden"`
	SecretNote string  `json:"secretNote" db:"secret_note" mfx:"writeonly"`
}

// policyServer mounts chartEntry with history behind a header identity, and runs
// the given policy on the DB step as the catalogue says to.
func policyServer(t *testing.T, p auth.Policy) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{chartEntry{}, maniflex.ModelConfig{Versioned: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Auth = &maniflex.AuthInfo{
					UserID: ctx.Request.Header.Get("X-User"),
					Claims: map[string]any{},
				}
				return next()
			})
			s.Pipeline.DB.Register(auth.Enforce(p))
		},
	})
}

func asPolicyUser(id string) map[string]string { return map[string]string{"X-User": id} }

// seedChart creates one entry owned by alice and returns its id.
func seedChart(t *testing.T, s *testutil.Server, extra map[string]any) string {
	t.Helper()
	body := map[string]any{
		"title": "mine", "ownerId": "alice", "orgId": "acme",
		"riskScore": 7, "secretNote": "s3cret",
	}
	for k, v := range extra {
		body[k] = v
	}
	return s.MustID(s.POST("/chart_entries", body, asPolicyUser("alice")))
}

// ── (a) one key space ─────────────────────────────────────────────────────────

func TestEnforcePolicyShape_OneKeySpaceOnEveryOperation(t *testing.T) {
	t.Parallel()
	seen := map[string][]string{}
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		op := string(ctx.Operation)
		if _, done := seen[op]; !done {
			keys := make([]string, 0, len(res))
			for k := range res {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			seen[op] = keys
		}
		return true, nil
	})

	id := seedChart(t, s, nil)
	s.GET("/chart_entries", asPolicyUser("alice"))
	s.GET("/chart_entries/"+id, asPolicyUser("alice"))
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "renamed"}, asPolicyUser("alice"))
	s.GET("/chart_entries/"+id+"/history", asPolicyUser("alice"))
	s.DELETE("/chart_entries/"+id, asPolicyUser("alice"))

	for _, op := range []string{"create", "list", "read", "update", "read_history", "delete"} {
		keys := seen[op]
		if keys == nil {
			t.Errorf("the policy was never called for %s", op)
			continue
		}
		joined := strings.Join(keys, " ")
		if !strings.Contains(joined, "ownerId") {
			t.Errorf("%s: no JSON-named key, got %v", op, keys)
		}
		for _, dbName := range []string{"owner_id", "org_id", "is_archived",
			"risk_score", "secret_note"} {
			if strings.Contains(joined, dbName) {
				t.Errorf("%s: DB column name %q reached the policy, got %v", op, dbName, keys)
			}
		}
	}
}

// The rule the finding names, run end to end. A negative check on a camelCase
// field was vacuously true on exactly the operations it existed for, so an
// archived record could still be updated and deleted.
func TestEnforcePolicyShape_NegativeRuleFreezesArchivedRecords(t *testing.T) {
	t.Parallel()
	s := policyServer(t, func(_ *maniflex.ServerContext, res map[string]any) (bool, error) {
		return res["isArchived"] != true, nil
	})

	id := seedChart(t, s, nil)
	// Archive it. The policy sees the row as stored — still unarchived — so this
	// one is allowed, which is the documented update semantics.
	s.PATCH("/chart_entries/"+id, map[string]any{"isArchived": true}, asPolicyUser("alice")).
		AssertStatus(http.StatusOK)

	// Now it is frozen. Both of these used to answer 200/204.
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "sneaky"}, asPolicyUser("alice")).
		AssertStatus(http.StatusForbidden)
	s.DELETE("/chart_entries/"+id, asPolicyUser("alice")).
		AssertStatus(http.StatusForbidden)
	s.GET("/chart_entries/"+id, asPolicyUser("alice")).
		AssertStatus(http.StatusForbidden)

	// And the record really is still there, so the refusals were not cosmetic.
	if n := len(s.GET("/chart_entries", asPolicyUser("alice")).DataList()); n != 0 {
		t.Errorf("the archived row should also be filtered out of the list, got %d row(s)", n)
	}
}

// MUST STILL WORK: a positive rule. It used to fail closed on the DB-keyed half
// — refusing the owner their own record — which at least got noticed; it must
// now allow the owner and refuse everyone else, on every operation.
func TestEnforcePolicyShape_PositiveRuleMatchesOnEveryOperation(t *testing.T) {
	t.Parallel()
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		return res["ownerId"] == ctx.Auth.UserID, nil
	})

	id := seedChart(t, s, nil)
	alice, mallory := asPolicyUser("alice"), asPolicyUser("mallory")

	s.GET("/chart_entries/"+id, alice).AssertStatus(http.StatusOK)
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "renamed"}, alice).
		AssertStatus(http.StatusOK)
	s.GET("/chart_entries/"+id+"/history", alice).AssertStatus(http.StatusOK)
	if n := len(s.GET("/chart_entries", alice).DataList()); n != 1 {
		t.Errorf("alice should see her own row in the list, got %d", n)
	}

	s.GET("/chart_entries/"+id, mallory).AssertStatus(http.StatusForbidden)
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "stolen"}, mallory).
		AssertStatus(http.StatusForbidden)
	s.DELETE("/chart_entries/"+id, mallory).AssertStatus(http.StatusForbidden)
	if n := len(s.GET("/chart_entries", mallory).DataList()); n != 0 {
		t.Errorf("mallory should see no rows, got %d", n)
	}
	s.DELETE("/chart_entries/"+id, alice).AssertStatus(http.StatusNoContent)
}

// ── (b) one value shape ───────────────────────────────────────────────────────

// A nullable column reached the DB-keyed branches as a *string, so a policy
// comparing it to a string could never match there — the same shape that made
// RequireOwner 404 the record's own owner (AUTH-19).
func TestEnforcePolicyShape_NullableColumnIsDereferenced(t *testing.T) {
	t.Parallel()
	types := map[string]string{}
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		op := string(ctx.Operation)
		if _, done := types[op]; !done {
			types[op] = typeOf(res["orgId"])
		}
		return true, nil
	})

	id := seedChart(t, s, nil)
	s.GET("/chart_entries", asPolicyUser("alice"))
	s.GET("/chart_entries/"+id, asPolicyUser("alice"))
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "renamed"}, asPolicyUser("alice"))
	s.DELETE("/chart_entries/"+id, asPolicyUser("alice"))

	for _, op := range []string{"create", "list", "read", "update", "delete"} {
		if types[op] != "string" {
			t.Errorf("%s: orgId is a %s, want string", op, types[op])
		}
	}
}

// A NULL column is nil, not a nil pointer — the same thing the serialised view
// shows, so a policy's "is it set" check reads the same on every operation.
func TestEnforcePolicyShape_NullColumnIsNil(t *testing.T) {
	t.Parallel()
	var onRead any = "policy not called"
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		if ctx.Operation == maniflex.OpRead {
			onRead = res["orgId"]
		}
		return true, nil
	})

	id := s.MustID(s.POST("/chart_entries",
		map[string]any{"title": "no org", "ownerId": "alice"}, asPolicyUser("alice")))
	s.GET("/chart_entries/"+id, asPolicyUser("alice")).AssertStatus(http.StatusOK)

	if onRead != nil {
		t.Errorf("orgId on read: got %v (%s), want nil", onRead, typeOf(onRead))
	}
}

// ── (c) every field, on every operation ───────────────────────────────────────

// A policy decides who may touch a record, so a hidden or writeonly column is
// exactly the kind of fact it may need. The list branch used to read the
// serialised rows, which have both stripped, so such a rule saw nil there and
// the value everywhere else.
func TestEnforcePolicyShape_HiddenAndWriteOnlyFieldsReachThePolicy(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		op := string(ctx.Operation)
		if _, done := seen[op]; !done {
			seen[op] = typeOf(res["riskScore"]) + "/" + typeOf(res["secretNote"])
		}
		return true, nil
	})

	id := seedChart(t, s, nil)
	s.GET("/chart_entries", asPolicyUser("alice"))
	s.GET("/chart_entries/"+id, asPolicyUser("alice"))
	s.PATCH("/chart_entries/"+id, map[string]any{"title": "renamed"}, asPolicyUser("alice"))

	for _, op := range []string{"list", "read", "update"} {
		if seen[op] == "<nil>/<nil>" {
			t.Errorf("%s: neither the hidden nor the writeonly field reached the policy (%s)",
				op, seen[op])
		}
		if strings.HasSuffix(seen[op], "/<nil>") {
			t.Errorf("%s: the writeonly field did not reach the policy (%s)", op, seen[op])
		}
	}
}

// MUST STILL WORK: the client must not see what the policy sees. Handing the
// stored row to the policy must not put a hidden or writeonly column into the
// response, which is still the serialised one.
func TestEnforcePolicyShape_ResponseStillHidesThoseFields(t *testing.T) {
	t.Parallel()
	s := policyServer(t, func(*maniflex.ServerContext, map[string]any) (bool, error) {
		return true, nil
	})
	id := seedChart(t, s, nil)

	read := s.GET("/chart_entries/"+id, asPolicyUser("alice"))
	read.AssertStatus(http.StatusOK)
	for _, leaked := range []string{"riskScore", "secretNote", "risk_score", "secret_note"} {
		if strings.Contains(string(read.Body), leaked) {
			t.Errorf("the read response carries %q: %s", leaked, read.Body)
		}
	}
	list := s.GET("/chart_entries", asPolicyUser("alice"))
	for _, leaked := range []string{"riskScore", "secretNote"} {
		if strings.Contains(string(list.Body), leaked) {
			t.Errorf("the list response carries %q: %s", leaked, list.Body)
		}
	}
}

// ── (d) create is the one that is not a stored record ─────────────────────────

// Pinned as documentation rather than as a wish: on create there is no stored
// record, so the map is the parsed body — no id, no timestamps, and a writeonly
// field only because the client sent one. A rule that needs the stored shape
// cannot be written for create, and this is why.
func TestEnforcePolicyShape_CreateSeesTheBody(t *testing.T) {
	t.Parallel()
	var keys []string
	s := policyServer(t, func(ctx *maniflex.ServerContext, res map[string]any) (bool, error) {
		if ctx.Operation == maniflex.OpCreate && keys == nil {
			for k := range res {
				keys = append(keys, k)
			}
			sort.Strings(keys)
		}
		return true, nil
	})
	seedChart(t, s, nil)

	joined := strings.Join(keys, " ")
	if strings.Contains(joined, "id") && !strings.Contains(joined, "ownerId") {
		t.Fatalf("unexpected create keys: %v", keys)
	}
	for _, absent := range []string{"created_at", "updated_at"} {
		if strings.Contains(joined, absent) {
			t.Errorf("create should see the body, not a stored row; got %v", keys)
		}
	}
	if !strings.Contains(joined, "secretNote") {
		t.Errorf("create should carry the writeonly field the client sent; got %v", keys)
	}
}

// ── (e) the list pairing refuses rather than guesses ──────────────────────────

// The policy reads the stored rows and filters the serialised ones, so the two
// must line up. A Response middleware that changes the row count breaks that,
// and evaluating a policy against the wrong record decides one caller's access
// by another record's fields — so it is a 500 naming the problem instead.
func TestEnforcePolicyShape_MismatchedListRowsAreRefused(t *testing.T) {
	t.Parallel()
	s := testutil.NewServer(t, testutil.Options{
		Models: []any{chartEntry{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Auth = &maniflex.AuthInfo{UserID: "alice", Claims: map[string]any{}}
				return next()
			})
			srv.Pipeline.DB.Register(auth.Enforce(
				func(*maniflex.ServerContext, map[string]any) (bool, error) { return true, nil }))
			// Appends a row the DB never produced.
			srv.Pipeline.Response.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if err := next(); err != nil {
					return err
				}
				if ctx.Operation == maniflex.OpList && ctx.Response != nil {
					if rows, ok := ctx.Response.Data.([]any); ok {
						ctx.Response.Data = append(rows, map[string]any{"id": "ghost"})
					}
				}
				return nil
			}, maniflex.AtPosition(maniflex.Before))
		},
	})
	s.POST("/chart_entries", map[string]any{"title": "x", "ownerId": "alice"})

	resp := s.GET("/chart_entries")
	if resp.Status != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500 — an unpairable list must not be served", resp.Status)
	}
}

// typeOf names a value's dynamic type, for the shape assertions above.
func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "<nil>"
	case string:
		return "string"
	case *string:
		return "*string"
	case bool:
		return "bool"
	case int:
		return "int"
	case int64:
		return "int64"
	case float64:
		return "float64"
	}
	return "other"
}
