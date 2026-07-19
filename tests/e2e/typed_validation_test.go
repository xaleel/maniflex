package e2e

// MS-15: the typed helpers and ctx.GetModel went straight to the adapter, so the
// model's own value constraints — enum, min, max — were enforced for HTTP
// callers and nobody else. The same model accepted in Go what it answered 422
// for over HTTP.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type valAcct struct {
	maniflex.BaseModel
	Email   string `json:"email"    mfx:"required"`
	Role    string `json:"role"     mfx:"enum:user|admin,default:user"`
	Balance int    `json:"balance"  mfx:"min:0,max:100"`
	IsAdmin bool   `json:"is_admin" mfx:"readonly"`
}

// valServer runs an arbitrary programmatic write and reports the error text plus
// the resulting row, so a test can compare against the HTTP answer.
func valServer(t *testing.T, run func(ctx *maniflex.ServerContext) (any, error)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{valAcct{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/val_accts/run",
				Handler: func(ctx *maniflex.ServerContext) error {
					out, err := run(ctx)
					msg := ""
					if err != nil {
						msg = err.Error()
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK,
						Data: map[string]any{"err": msg, "out": out}}
					return nil
				},
			})
		},
	})
}

func runWrite(t *testing.T, srv *testutil.Server) map[string]any {
	t.Helper()
	return srv.POST("/val_accts/run", map[string]any{}).AssertStatus(http.StatusOK).Data()
}

// TestTypedValidation_CreateRejectsWhatHTTPRejects is the core case. Asserted
// against the HTTP endpoint's own verdict on the same values, so the two doors
// cannot drift apart again.
func TestTypedValidation_CreateRejectsWhatHTTPRejects(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		return maniflex.Create(ctx, &valAcct{
			Email: "a@b.c", Role: "superuser", Balance: 9999,
		})
	})

	httpResp := srv.POST("/val_accts", map[string]any{
		"email": "a@b.c", "role": "superuser", "balance": 9999,
	})
	if httpResp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("HTTP baseline: want 422, got %d", httpResp.Status)
	}

	got := runWrite(t, srv)
	msg, _ := got["err"].(string)
	if msg == "" {
		t.Fatalf("typed Create accepted what HTTP rejected with 422: %#v", got["out"])
	}
	// Both violations reported, not just the first — the same as HTTP's details.
	if !strings.Contains(msg, "must be one of") {
		t.Errorf("enum violation not reported: %s", msg)
	}
	if !strings.Contains(msg, "must be <= 100") {
		t.Errorf("max violation not reported: %s", msg)
	}
	if got["out"] != nil {
		t.Errorf("a rejected write must return no record: %#v", got["out"])
	}
}

// TestTypedValidation_NothingIsWritten pins that the check runs *before* the
// adapter call. Reporting an error after the row landed would be worse than not
// checking at all.
func TestTypedValidation_NothingIsWritten(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		return maniflex.Create(ctx, &valAcct{Email: "a@b.c", Role: "superuser"})
	})
	runWrite(t, srv)

	rows := srv.GET("/val_accts").AssertStatus(http.StatusOK).DataList()
	if len(rows) != 0 {
		t.Errorf("rejected create still wrote a row: %#v", rows)
	}
}

// TestTypedValidation_UpdateAndAccessorToo covers the other three entry points.
// Fixing only Create[T] would leave three doors open.
func TestTypedValidation_UpdateAndAccessorToo(t *testing.T) {
	t.Parallel()

	t.Run("typed_update", func(t *testing.T) {
		t.Parallel()
		srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
			c, err := maniflex.Create(ctx, &valAcct{Email: "a@b.c", Role: "user"})
			if err != nil {
				return nil, err
			}
			return maniflex.Update(ctx, c.ID, &valAcct{Email: "a@b.c", Role: "superuser"})
		})
		if msg, _ := runWrite(t, srv)["err"].(string); !strings.Contains(msg, "must be one of") {
			t.Errorf("typed Update accepted an invalid enum: %q", msg)
		}
	})

	t.Run("accessor_create", func(t *testing.T) {
		t.Parallel()
		srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
			return ctx.GetModel("valAcct").Create(map[string]any{
				"email": "a@b.c", "role": "superuser",
			})
		})
		if msg, _ := runWrite(t, srv)["err"].(string); !strings.Contains(msg, "must be one of") {
			t.Errorf("accessor Create accepted an invalid enum: %q", msg)
		}
	})

	t.Run("accessor_update", func(t *testing.T) {
		t.Parallel()
		srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
			c, err := ctx.GetModel("valAcct").Create(map[string]any{"email": "a@b.c", "role": "user"})
			if err != nil {
				return nil, err
			}
			return ctx.GetModel("valAcct").Update(c["id"].(string), map[string]any{"balance": 9999})
		})
		if msg, _ := runWrite(t, srv)["err"].(string); !strings.Contains(msg, "must be <= 100") {
			t.Errorf("accessor Update accepted an out-of-range value: %q", msg)
		}
	})
}

// TestTypedValidation_ReadonlyStaysWritable is the anti-over-reach test: the
// decision was to enforce value constraints but NOT readonly, because that tag
// means "not from a client" and a background job is not a client. A fix that
// copied the HTTP behaviour wholesale would strip is_admin and fail here.
func TestTypedValidation_ReadonlyStaysWritable(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		c, err := maniflex.Create(ctx, &valAcct{Email: "a@b.c", Role: "admin", IsAdmin: true})
		if err != nil {
			return nil, err
		}
		return c.IsAdmin, nil
	})

	got := runWrite(t, srv)
	if msg, _ := got["err"].(string); msg != "" {
		t.Fatalf("programmatic write of a readonly field should succeed: %s", msg)
	}
	if got["out"] != true {
		t.Errorf("readonly field not written programmatically: %#v", got["out"])
	}
	// And it is still refused from an HTTP client, which is what the tag is for.
	d := srv.POST("/val_accts", map[string]any{
		"email": "c@d.e", "role": "user", "is_admin": true,
	}).AssertStatus(http.StatusCreated).Data()
	if d["is_admin"] != false {
		t.Errorf("readonly field accepted from a client: %#v", d["is_admin"])
	}
}

// TestTypedValidation_ValidWriteUnaffected is the anti-vacuity pair: a check
// that rejected everything would pass every test above.
func TestTypedValidation_ValidWriteUnaffected(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		c, err := maniflex.Create(ctx, &valAcct{Email: "a@b.c", Role: "admin", Balance: 50})
		if err != nil {
			return nil, err
		}
		return map[string]any{"role": c.Role, "balance": c.Balance}, nil
	})

	got := runWrite(t, srv)
	if msg, _ := got["err"].(string); msg != "" {
		t.Fatalf("valid write rejected: %s", msg)
	}
	out := got["out"].(map[string]any)
	if out["role"] != "admin" || out["balance"] != float64(50) {
		t.Errorf("valid write mangled: %#v", out)
	}
}

// TestTypedValidation_SkipValidationEscapeHatch covers the documented way out,
// for a violation that is deliberate — backfilling rows that predate an enum.
func TestTypedValidation_SkipValidationEscapeHatch(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		c, err := maniflex.Create(ctx, &valAcct{Email: "a@b.c", Role: "legacy"},
			maniflex.SkipValidation())
		if err != nil {
			return nil, err
		}
		return c.Role, nil
	})

	got := runWrite(t, srv)
	if msg, _ := got["err"].(string); msg != "" {
		t.Fatalf("SkipValidation did not skip: %s", msg)
	}
	if got["out"] != "legacy" {
		t.Errorf("SkipValidation: got %#v, want legacy stored", got["out"])
	}
}

// TestTypedValidation_OmittedDefaultedFieldNotJudged is the MS-13 interaction.
// Role is zero AND carries default:user, so createPresent drops it from the
// INSERT — validating it anyway would reject "" against the enum and make an
// ordinary create impossible. The check is limited to the columns actually sent.
func TestTypedValidation_OmittedDefaultedFieldNotJudged(t *testing.T) {
	t.Parallel()

	srv := valServer(t, func(ctx *maniflex.ServerContext) (any, error) {
		c, err := maniflex.Create(ctx, &valAcct{Email: "a@b.c"}) // Role left empty
		if err != nil {
			return nil, err
		}
		return c.Role, nil
	})

	got := runWrite(t, srv)
	if msg, _ := got["err"].(string); msg != "" {
		t.Fatalf("an omitted defaulted field must not be validated: %s", msg)
	}
	if got["out"] != "user" {
		t.Errorf("expected the column default to apply: %#v", got["out"])
	}
}
