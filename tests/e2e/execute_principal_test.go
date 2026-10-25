package e2e

// Audit AUTH-22: Server.Execute runs on a copy of the caller's principal.
//
// It used to run on the pointer, which is the natural thing to write and wrong
// in two ways at once. A principal built without Claims — the shape Execute's
// own doc example uses — panicked the first middleware that enriched it, and
// nothing recovered it: PanicRecoverer is HTTP middleware around the router and
// Execute calls the pipeline directly, so the panic escaped Execute into the
// caller's own frame. And a principal reused across invocations, which is what
// the maker-checker loop in the docs does, carried each invocation's enrichment
// into the next: a role granted for one invocation was still granted for the
// one after it, so a RequireRole gate opened for a caller that never qualified.
//
// The deliberate loss is asserted too (TheCallersStructIsNotMutated): an app
// could read pipeline enrichment back out of its own struct afterwards, and now
// cannot. Undocumented, unavailable over HTTP, and the price of the above.
//
//	go test ./tests/e2e/... -run TestExecutePrincipal

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ledgerEntry struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
}

// execPrincipalServer registers mw on the Auth step, in order.
func execPrincipalServer(t *testing.T, mw ...maniflex.MiddlewareFunc) *maniflex.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{ledgerEntry{}},
		Middleware: func(s *maniflex.Server) {
			for _, m := range mw {
				s.Pipeline.Auth.Register(m)
			}
		},
	}).ManiflexServer()
}

func execCreate(t *testing.T, mfx *maniflex.Server, inv maniflex.Invocation) *maniflex.APIResponse {
	t.Helper()
	inv.Model, inv.Operation = "ledgerEntry", maniflex.OpCreate
	if inv.Body == nil {
		inv.Body = map[string]any{"title": "written in process"}
	}
	res, err := mfx.Execute(t.Context(), inv)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

// AuthInfo.Claims is documented "always non-nil when Auth is set", so middleware
// writes to it without checking. Execute was the one path that did not honour
// that, and the result was a nil-map panic nothing on this path recovers.
func TestExecutePrincipal_NilClaimsIsFilled(t *testing.T) {
	var sawMap bool
	enrich := func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth != nil {
			sawMap = ctx.Auth.Claims != nil
			ctx.Auth.Claims["enriched"] = true // panicked before AUTH-22
		}
		return next()
	}
	mfx := execPrincipalServer(t, enrich)

	// No Claims — the shape Execute's doc example and the maker-checker loop use.
	res := execCreate(t, mfx, maniflex.Invocation{Auth: &maniflex.AuthInfo{UserID: "svc-1"}})

	if !sawMap {
		t.Fatal("middleware saw a nil Claims map; the AuthInfo.Claims contract says " +
			"it is always non-nil when Auth is set")
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.StatusCode)
	}
}

// Claims written by one invocation must not be the next one's starting state.
func TestExecutePrincipal_ClaimsDoNotCarryBetweenInvocations(t *testing.T) {
	var onEntry []any
	enrich := func(ctx *maniflex.ServerContext, next func() error) error {
		onEntry = append(onEntry, ctx.Auth.Claims["caller"])
		ctx.Auth.Claims["caller"] = "seen"
		return next()
	}
	mfx := execPrincipalServer(t, enrich)

	principal := &maniflex.AuthInfo{UserID: "svc-1", Claims: map[string]any{}}
	for i := 0; i < 3; i++ {
		execCreate(t, mfx, maniflex.Invocation{Auth: principal})
	}

	for i, v := range onEntry {
		if v != nil {
			t.Fatalf("invocation %d began with Claims[caller] = %v, inherited from the "+
				"invocation before it; each invocation runs on its own copy", i+1, v)
		}
	}
}

// The same inheritance, but decided by an authorisation gate rather than
// observed in a log line. A middleware grants a role for one kind of invocation;
// the invocation after it must not keep the role.
func TestExecutePrincipal_GrantedRoleDoesNotCarryToTheNextInvocation(t *testing.T) {
	grantForImport := func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth != nil && ctx.Request.Header.Get("X-Bulk-Import") == "1" {
			ctx.Auth.Roles = append(ctx.Auth.Roles, "importer")
		}
		return next()
	}
	mfx := execPrincipalServer(t, grantForImport, auth.RequireRole("importer"))

	principal := &maniflex.AuthInfo{UserID: "svc-1", Claims: map[string]any{}}

	// A bulk import: the middleware grants the role, so the gate opens.
	execCreate(t, mfx, maniflex.Invocation{
		Auth:   principal,
		Header: http.Header{"X-Bulk-Import": {"1"}},
	})

	// An ordinary write by the same principal. Nothing grants the role here.
	_, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "ledgerEntry", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "ordinary"}, Auth: principal,
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 ExecuteError — the role granted to the previous "+
			"invocation opened the gate for this one", err)
	}
}

// Scopes carry the same way Roles do, and auth.RequireScope reads them.
func TestExecutePrincipal_GrantedScopeDoesNotCarryToTheNextInvocation(t *testing.T) {
	grantForImport := func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Auth != nil && ctx.Request.Header.Get("X-Bulk-Import") == "1" {
			ctx.Auth.Scopes = append(ctx.Auth.Scopes, "ledger:import")
		}
		return next()
	}
	mfx := execPrincipalServer(t, grantForImport, auth.RequireScope("ledger:import"))

	principal := &maniflex.AuthInfo{UserID: "svc-1", Claims: map[string]any{}}
	execCreate(t, mfx, maniflex.Invocation{
		Auth:   principal,
		Header: http.Header{"X-Bulk-Import": {"1"}},
	})

	_, err := mfx.Execute(t.Context(), maniflex.Invocation{
		Model: "ledgerEntry", Operation: maniflex.OpCreate,
		Body: map[string]any{"title": "ordinary"}, Auth: principal,
	})
	var ee *maniflex.ExecuteError
	if !errors.As(err, &ee) || ee.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 ExecuteError — the scope granted to the previous "+
			"invocation opened the gate for this one", err)
	}
}

// The behaviour change, pinned on purpose: the caller's struct is left alone.
// An app could read middleware enrichment back out of its own principal after
// an Execute. That was never documented and has no equivalent over HTTP, and it
// is the same aliasing as the two tests above, so it goes with them.
func TestExecutePrincipal_TheCallersStructIsNotMutated(t *testing.T) {
	enrich := func(ctx *maniflex.ServerContext, next func() error) error {
		ctx.Auth.Claims["enriched"] = true
		ctx.Auth.Roles = append(ctx.Auth.Roles, "granted")
		ctx.Auth.TenantID = "resolved-by-middleware"
		return next()
	}
	mfx := execPrincipalServer(t, enrich)

	principal := &maniflex.AuthInfo{UserID: "svc-1", Roles: []string{"base"}}
	execCreate(t, mfx, maniflex.Invocation{Auth: principal})

	if principal.Claims != nil {
		t.Errorf("caller's Claims = %v, want nil — Execute filled the map in the "+
			"caller's own struct instead of in its copy", principal.Claims)
	}
	if !reflect.DeepEqual(principal.Roles, []string{"base"}) {
		t.Errorf("caller's Roles = %v, want [base]", principal.Roles)
	}
	if principal.TenantID != "" {
		t.Errorf("caller's TenantID = %q, want empty", principal.TenantID)
	}
}

// MUST STILL WORK: copying must not drop anything. Every field is set, and
// every one has to arrive — the copy is what the gates then read.
func TestExecutePrincipal_EveryFieldReachesThePipeline(t *testing.T) {
	want := maniflex.AuthInfo{
		UserID:       "svc-1",
		Roles:        []string{"admin", "auditor"},
		Claims:       map[string]any{"org_id": "acme", "nested": map[string]any{"k": "v"}},
		TenantID:     "acme",
		IdentityType: maniflex.IdentityServiceAccount,
		Scopes:       []string{"ledger:read", "ledger:write"},
		SessionID:    "jti-1",
		AuthMethod:   "api_key",
	}
	var got *maniflex.AuthInfo
	capture := func(ctx *maniflex.ServerContext, next func() error) error {
		got = ctx.Auth
		return next()
	}
	mfx := execPrincipalServer(t, capture)

	execCreate(t, mfx, maniflex.Invocation{Auth: &want})

	if got == nil {
		t.Fatal("the pipeline saw no principal")
	}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("principal reached the pipeline as\n  %+v\nwant\n  %+v", *got, want)
	}
}

// MUST STILL WORK: nil in, nil out. An unauthenticated invocation must reach the
// pipeline with no principal at all, not a zero-value one — every authenticator
// and AllowAnonymous decides on ctx.Auth == nil, so a zero principal would make
// an anonymous in-process call look authenticated as "".
func TestExecutePrincipal_NilAuthStaysNil(t *testing.T) {
	var sawPrincipal bool
	capture := func(ctx *maniflex.ServerContext, next func() error) error {
		sawPrincipal = ctx.Auth != nil
		return next()
	}
	mfx := execPrincipalServer(t, capture)

	execCreate(t, mfx, maniflex.Invocation{}) // Auth deliberately nil

	if sawPrincipal {
		t.Fatal("an invocation with no principal reached the pipeline carrying one; " +
			"gates that test ctx.Auth == nil would treat it as authenticated")
	}
}

// MUST STILL WORK, and future-proof: the copy must break the aliasing of every
// reference-typed field AuthInfo has, found by reflection rather than listed.
// A field added to AuthInfo later and forgotten in clone fails here — the one
// test that survives that change, since a scalar field copies for free and a
// new slice or map would not.
func TestExecutePrincipal_NoReferenceFieldIsShared(t *testing.T) {
	seed, mutate := seededPrincipal(t)

	var mutated []string
	mutator := func(ctx *maniflex.ServerContext, next func() error) error {
		mutated = mutate(ctx.Auth)
		return next()
	}
	mfx := execPrincipalServer(t, mutator)

	// Two independently seeded principals: one handed to Execute, one kept as
	// the snapshot. A struct copy would share the very slices under test.
	principal, before := seed(), seed()
	execCreate(t, mfx, maniflex.Invocation{Auth: &principal})

	if len(mutated) == 0 {
		t.Fatal("no reference-typed field was exercised; the reflection walk below " +
			"found nothing to mutate, so this test is asserting nothing")
	}
	t.Logf("mutated in the copy: %v", mutated)
	if !reflect.DeepEqual(principal, before) {
		t.Fatalf("the caller's principal changed when the pipeline wrote to its copy:\n"+
			"  after: %+v\n  before: %+v\nsome reference-typed field of AuthInfo is "+
			"still shared — add it to (*AuthInfo).clone", principal, before)
	}
}

// seededPrincipal returns a builder for a principal with every slice and map
// field populated, and a function that mutates each of those fields of another
// principal in place, reporting which ones it touched. In-place is the point:
// appending to a slice may reallocate and hide the sharing, while writing to
// index 0 cannot.
func seededPrincipal(t *testing.T) (func() maniflex.AuthInfo, func(*maniflex.AuthInfo) []string) {
	t.Helper()
	seed := func() maniflex.AuthInfo {
		var seeded maniflex.AuthInfo
		rv := reflect.ValueOf(&seeded).Elem()
		for i := 0; i < rv.NumField(); i++ {
			f, name := rv.Field(i), rv.Type().Field(i).Name
			switch f.Kind() {
			case reflect.Slice:
				if f.Type().Elem().Kind() != reflect.String {
					t.Fatalf("AuthInfo.%s is a slice of %s; seed it here and extend mutate",
						name, f.Type().Elem())
				}
				f.Set(reflect.ValueOf([]string{"seed-" + name}))
			case reflect.Map:
				if f.Type().Key().Kind() != reflect.String {
					t.Fatalf("AuthInfo.%s is a map keyed by %s; seed it here and extend mutate",
						name, f.Type().Key())
				}
				f.Set(reflect.MakeMap(f.Type()))
				f.SetMapIndex(reflect.ValueOf("seed"), reflect.Zero(f.Type().Elem()))
			}
		}
		return seeded
	}

	mutate := func(a *maniflex.AuthInfo) []string {
		var touched []string
		mv := reflect.ValueOf(a).Elem()
		for i := 0; i < mv.NumField(); i++ {
			f, name := mv.Field(i), mv.Type().Field(i).Name
			switch f.Kind() {
			case reflect.Slice:
				if f.Len() > 0 {
					f.Index(0).SetString("mutated")
					touched = append(touched, name)
				}
			case reflect.Map:
				f.SetMapIndex(reflect.ValueOf("mutated"), reflect.Zero(f.Type().Elem()))
				touched = append(touched, name)
			}
		}
		return touched
	}
	return seed, mutate
}
