package e2e

// Audit STEP-4 — Deserialize decodes the whole client body into ctx.Record
// before Validate runs, and Validate's strip called DeleteField, which removed
// the key from the body and the column from the present set but left the struct
// field holding what the client sent.
//
// The written row was always correct, because recordToMap honours the present
// set. What was wrong is what the application sees: For, Bind, Handle and
// ctx.Record handed middleware an attacker-chosen Role, Balance or TenantID with
// nothing marking it refused — and Create[T], which deliberately trusts its
// caller instead of re-applying readonly, would persist it.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type StrippedUser struct {
	maniflex.BaseModel
	Email   string `json:"email"   db:"email"   mfx:"required"`
	Role    string `json:"role"    db:"role"    mfx:"readonly"`
	Balance int    `json:"balance" db:"balance" mfx:"readonly"`
	Tenant  string `json:"tenant"  db:"tenant"  mfx:"immutable"`
}

// seen captures what a Service-step middleware written in the documented typed
// style is handed, which is the whole point of the finding.
type seen struct {
	role    string
	balance int
	tenant  string
	ran     bool
}

func strippedSrv(t *testing.T, got *seen) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{StrippedUser{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, u *StrippedUser) error {
					got.role, got.balance, got.tenant = u.Role, u.Balance, u.Tenant
					got.ran = true
					return nil
				}),
				maniflex.ForModel("StrippedUser"),
			)
		},
	})
}

func TestStrippedRecord_ReadonlyValuesDoNotReachMiddleware(t *testing.T) {
	t.Parallel()
	var got seen
	srv := strippedSrv(t, &got)

	resp := srv.POST("/stripped_users", map[string]any{
		"email":   "a@b.c",
		"role":    "admin",
		"balance": 999999,
	})
	resp.AssertStatus(http.StatusCreated)

	if !got.ran {
		t.Fatal("the typed middleware never ran; this test asserts nothing")
	}
	if got.role != "" {
		t.Errorf("middleware saw Role = %q; Validate refused the client's value, so the "+
			"record must not still carry it", got.role)
	}
	if got.balance != 0 {
		t.Errorf("middleware saw Balance = %d, want 0", got.balance)
	}
	// The row was always right — assert it stays right.
	if v := resp.Data()["role"]; v != "" {
		t.Errorf("stored role = %#v, want empty", v)
	}
}

// Go's decoder matches keys case-insensitively, so a client who cannot spell the
// field the documented way still reaches the struct field.
func TestStrippedRecord_CaseInsensitiveKeyIsAlsoStripped(t *testing.T) {
	t.Parallel()
	var got seen
	srv := strippedSrv(t, &got)

	srv.POST("/stripped_users", map[string]any{"email": "a@b.c", "ROLE": "admin"}).
		AssertStatus(http.StatusCreated)

	if got.role != "" {
		t.Errorf(`middleware saw Role = %q from {"ROLE": ...}`, got.role)
	}
}

// Immutable is the operation-dependent one: settable on create, refused on
// update. The record must follow that distinction rather than being blanked.
func TestStrippedRecord_ImmutableSurvivesCreateAndIsStrippedOnUpdate(t *testing.T) {
	t.Parallel()
	var got seen
	srv := strippedSrv(t, &got)

	resp := srv.POST("/stripped_users", map[string]any{
		"email": "a@b.c", "tenant": "acme",
	})
	resp.AssertStatus(http.StatusCreated)
	if got.tenant != "acme" {
		t.Errorf("middleware saw Tenant = %q on create; immutable is settable there", got.tenant)
	}

	got = seen{}
	srv.PATCH("/stripped_users/"+resp.ID(), map[string]any{"tenant": "victim"}).
		AssertStatus(http.StatusOK)
	if got.tenant != "" {
		t.Errorf("middleware saw Tenant = %q on update; immutable is refused there", got.tenant)
	}
	if v := srv.GET("/stripped_users/" + resp.ID()).Data()["tenant"]; v != "acme" {
		t.Errorf("stored tenant = %#v, want %q — the update must not have moved it", v, "acme")
	}
}

// The strip must not become a blanket wipe: a field the client is allowed to
// send still reaches the middleware and the row.
func TestStrippedRecord_AllowedFieldsStillReachMiddleware(t *testing.T) {
	t.Parallel()
	var got seen
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{StrippedUser{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, u *StrippedUser) error {
					got.role = u.Email // reuse the field to carry the observation
					got.ran = true
					return nil
				}),
				maniflex.ForModel("StrippedUser"),
			)
		},
	})

	srv.POST("/stripped_users", map[string]any{"email": "keep@me.com"}).
		AssertStatus(http.StatusCreated)
	if got.role != "keep@me.com" {
		t.Errorf("middleware saw Email = %q, want the value the client legitimately sent", got.role)
	}
}

// A value the *server* stamps into a readonly column is not from a client, and
// the strip has always exempted it. Zeroing must not break that exemption.
func TestStrippedRecord_ServerStampedReadonlyIsKept(t *testing.T) {
	t.Parallel()
	var got seen
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{StrippedUser{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.SetField("role", "stamped-by-server")
				return next()
			}, maniflex.ForModel("StrippedUser"))
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, u *StrippedUser) error {
					got.role = u.Role
					got.ran = true
					return nil
				}),
				maniflex.ForModel("StrippedUser"),
			)
		},
	})

	resp := srv.POST("/stripped_users", map[string]any{"email": "a@b.c", "role": "admin"})
	resp.AssertStatus(http.StatusCreated)

	if got.role != "stamped-by-server" {
		t.Errorf("middleware saw Role = %q, want the server's stamp — a readonly value "+
			"the server set is not a client value and must survive the strip", got.role)
	}
	if v := resp.Data()["role"]; v != "stamped-by-server" {
		t.Errorf("stored role = %#v, want the server's stamp", v)
	}
}

// The strip happens in Validate, so a Deserialize-step middleware still sees the
// raw request. That is the documented seam for code that wants to know what a
// client attempted — middleware.md says so, and this pins it.
func TestStrippedRecord_DeserializeStepStillSeesTheRawBody(t *testing.T) {
	t.Parallel()
	var atDeserialize, atService seen
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{StrippedUser{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, u *StrippedUser) error {
					atDeserialize.role, atDeserialize.ran = u.Role, true
					return nil
				}),
				maniflex.ForModel("StrippedUser"),
				maniflex.AtPosition(maniflex.After),
			)
			s.Pipeline.Service.Register(
				maniflex.Handle(func(ctx *maniflex.ServerContext, u *StrippedUser) error {
					atService.role, atService.ran = u.Role, true
					return nil
				}),
				maniflex.ForModel("StrippedUser"),
			)
		},
	})

	srv.POST("/stripped_users", map[string]any{"email": "a@b.c", "role": "admin"}).
		AssertStatus(http.StatusCreated)

	if !atDeserialize.ran || !atService.ran {
		t.Fatalf("middleware did not run: deserialize=%v service=%v",
			atDeserialize.ran, atService.ran)
	}
	if atDeserialize.role != "admin" {
		t.Errorf("Deserialize saw Role = %q, want %q — the strip runs in Validate, so "+
			"this is where a client's attempt is still visible", atDeserialize.role, "admin")
	}
	if atService.role != "" {
		t.Errorf("Service saw Role = %q, want empty", atService.role)
	}
}
