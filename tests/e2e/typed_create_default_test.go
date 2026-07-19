package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// defDoc carries one field of each shape the MS-13 rule has to distinguish:
// a defaulted string and int, a defaulted pointer, and an undefaulted int.
type defDoc struct {
	maniflex.BaseModel
	Name     string `json:"name"     mfx:"required"`
	Status   string `json:"status"   mfx:"default:active"`
	Priority int    `json:"priority" mfx:"default:5"`
	Score    *int   `json:"score"    mfx:"default:7"`
	Count    int    `json:"count"`
}

// defDocServer mounts an action that runs a caller-supplied typed Create and
// echoes the stored row back, so a test can compare it against an HTTP create
// of the same model in the same server.
func defDocServer(t *testing.T, build func() *defDoc) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{defDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/def_docs/typed",
				Handler: func(ctx *maniflex.ServerContext) error {
					out, err := maniflex.Create(ctx, build())
					if err != nil {
						return err
					}
					score := any(nil)
					if out.Score != nil {
						score = *out.Score
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{
						"id": out.ID, "status": out.Status, "priority": out.Priority,
						"count": out.Count, "score": score,
					}}
					return nil
				},
			})
		},
	})
}

func typedCreate(t *testing.T, srv *testutil.Server) map[string]any {
	t.Helper()
	return srv.POST("/def_docs/typed", map[string]any{}).AssertStatus(http.StatusOK).Data()
}

// TestTypedCreate_DefaultsMatchHTTP is the MS-13 regression. A default: tag is
// only ever a SQL DEFAULT clause, so it fires only when the INSERT omits the
// column — which Create[T] never did. The assertion is against the HTTP create
// of the same model rather than against literals, because the complaint is the
// divergence, not either value on its own.
func TestTypedCreate_DefaultsMatchHTTP(t *testing.T) {
	t.Parallel()

	srv := defDocServer(t, func() *defDoc { return &defDoc{Name: "typed"} })

	httpDoc := srv.POST("/def_docs", map[string]any{"name": "http"}).
		AssertStatus(http.StatusCreated).Data()
	typed := typedCreate(t, srv)

	if httpDoc["status"] != typed["status"] {
		t.Errorf("status: http=%v typed=%v", httpDoc["status"], typed["status"])
	}
	if httpDoc["priority"] != typed["priority"] {
		t.Errorf("priority: http=%v typed=%v", httpDoc["priority"], typed["priority"])
	}
	if httpDoc["score"] != typed["score"] {
		t.Errorf("score: http=%v typed=%v", httpDoc["score"], typed["score"])
	}
	// Pin the values too: if a future change stopped applying defaults on BOTH
	// paths the comparison above would still pass.
	if typed["status"] != "active" || typed["priority"] != float64(5) {
		t.Errorf("defaults not applied: status=%v priority=%v", typed["status"], typed["priority"])
	}
}

// TestTypedCreate_ExplicitValueWins is the anti-vacuity pair: a fix that simply
// omitted every defaulted column would pass the test above and fail this one.
func TestTypedCreate_ExplicitValueWins(t *testing.T) {
	t.Parallel()

	srv := defDocServer(t, func() *defDoc {
		return &defDoc{Name: "typed", Status: "archived", Priority: 9}
	})
	typed := typedCreate(t, srv)

	if typed["status"] != "archived" {
		t.Errorf("explicit status: got %v, want archived", typed["status"])
	}
	if typed["priority"] != float64(9) {
		t.Errorf("explicit priority: got %v, want 9", typed["priority"])
	}
}

// TestTypedCreate_UndefaultedZeroIsStillWritten is the line between the narrow
// rule and the broad one. Count carries no default: tag, so its zero is a value
// the caller stated, not an omission — under "skip every zero-valued field" it
// would have been dropped from the INSERT instead.
func TestTypedCreate_UndefaultedZeroIsStillWritten(t *testing.T) {
	t.Parallel()

	srv := defDocServer(t, func() *defDoc { return &defDoc{Name: "typed", Count: 0} })
	typed := typedCreate(t, srv)

	if typed["count"] != float64(0) {
		t.Errorf("undefaulted zero: got %v, want 0 written", typed["count"])
	}
}

// TestTypedCreate_PointerIsTheEscapeHatch covers the documented way to store an
// explicit zero in a defaulted column: nil takes the default, a pointer to zero
// writes the zero. Without this a defaulted column could never hold its zero.
func TestTypedCreate_PointerIsTheEscapeHatch(t *testing.T) {
	t.Parallel()

	t.Run("nil_takes_the_default", func(t *testing.T) {
		t.Parallel()
		srv := defDocServer(t, func() *defDoc { return &defDoc{Name: "typed"} })
		if got := typedCreate(t, srv)["score"]; got != float64(7) {
			t.Errorf("nil *int: got %v, want the default 7", got)
		}
	})

	t.Run("pointer_to_zero_writes_zero", func(t *testing.T) {
		t.Parallel()
		srv := defDocServer(t, func() *defDoc {
			zero := 0
			return &defDoc{Name: "typed", Score: &zero}
		})
		if got := typedCreate(t, srv)["score"]; got != float64(0) {
			t.Errorf("&0: got %v, want 0 written over the default", got)
		}
	})
}

// TestTypedCreate_StampsIDOnCallersRecord guards the side effect the fix had to
// preserve: Create installs a present set on the caller's record rather than on
// a copy, so the id the insert builder generates is still visible to the caller
// afterwards. A copy would have been the tidier implementation and would have
// silently dropped this.
func TestTypedCreate_StampsIDOnCallersRecord(t *testing.T) {
	t.Parallel()

	var idOnRecord, idReturned string
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{defDoc{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/def_docs/stamp",
				Handler: func(ctx *maniflex.ServerContext) error {
					rec := &defDoc{Name: "typed"}
					out, err := maniflex.Create(ctx, rec)
					if err != nil {
						return err
					}
					idOnRecord, idReturned = rec.ID, out.ID
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})
	srv.POST("/def_docs/stamp", map[string]any{}).AssertStatus(http.StatusOK)

	if idOnRecord == "" || idOnRecord != idReturned {
		t.Errorf("caller's record id: got %q, want the created id %q", idOnRecord, idReturned)
	}
}
