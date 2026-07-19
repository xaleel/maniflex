package e2e

// Regression tests for the MS-L1..MS-L14 sweep. Grouped in one file because each
// item is small; the shared theme is that every one of them failed silently.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── MS-L1 / MS-L2 / MS-L11 / MS-L12: registration-time refusals ──────────────

// lCycleA/lCycleB are mutually embedded through pointers, which overflowed the
// stack during registration (MS-L1).
//
// Worth noting what fires: Go forbids a *value*-embedded cycle outright
// (illegal recursive type), so a pointer embed is the only way to express one —
// and MS-L2's refusal of pointer embeds now rejects this first. The visited set
// added for MS-L1 is therefore belt-and-braces rather than the load-bearing
// fix, and this case asserts the refusal, not which of the two produced it.
type lCycleA struct {
	maniflex.BaseModel
	*lCycleB
	Name string `json:"name"`
}
type lCycleB struct {
	*lCycleA
	Note string `json:"note"`
}

type lPtrBase struct {
	*maniflex.BaseModel
	Name string `json:"name"`
}

type lBadMin struct {
	maniflex.BaseModel
	Qty int `json:"qty" mfx:"min:abc"`
}

type lBadEnum struct {
	maniflex.BaseModel
	Status string `json:"status" mfx:"enum:draft||live"`
}

type lReadonlyRequired struct {
	maniflex.BaseModel
	Token string `json:"token" mfx:"readonly,required"`
}

type lDupColumn struct {
	maniflex.BaseModel
	Ref   string `json:"ref"   db:"shared"`
	Other string `json:"other" db:"shared"`
}

// TestAuditLows_RegistrationRefusals covers the items whose whole fix is a clear
// boot error instead of a crash, a silent no-op, or an ambiguity.
func TestAuditLows_RegistrationRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		model any
		want  string
	}{
		// MS-L1: recursed forever and overflowed the stack at boot. The pointer
		// refusal below now catches this first — see the note on lCycleA.
		{"mutual_pointer_embed", lCycleA{}, "by pointer"},
		// MS-L2: registered fine, then panicked on the first request.
		{"pointer_embedded_base", lPtrBase{}, "by pointer"},
		// MS-L11: parsed as a constraint, enforced nothing.
		{"unparseable_min", lBadMin{}, "unusable mfx values"},
		{"empty_enum_member", lBadEnum{}, "unusable mfx values"},
		// MS-L11: stripped before the required check, so never required.
		{"readonly_required", lReadonlyRequired{}, "readonly"},
		// MS-L12: first-wins on read, last-wins on write.
		{"duplicate_db_name", lDupColumn{}, "column"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
			err := srv.Register(tc.model)
			if err == nil {
				t.Fatalf("expected registration to be refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestAuditLows_ValidModelsStillRegister is the anti-vacuity pair: the refusals
// above must not have made ordinary models unregisterable.
func TestAuditLows_ValidModelsStillRegister(t *testing.T) {
	t.Parallel()

	type lFine struct {
		maniflex.BaseModel
		Qty    int    `json:"qty"    mfx:"min:0,max:10"`
		Status string `json:"status" mfx:"enum:draft|live,default:draft"`
		Token  string `json:"token"  mfx:"writeonly,required"`
		Owner  string `json:"owner"  mfx:"readonly"`
	}
	srv := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
	if err := srv.Register(lFine{}); err != nil {
		t.Fatalf("a valid model must still register: %v", err)
	}
}

// ── MS-L3: Aggregate direction ───────────────────────────────────────────────

// TestAuditLows_AggregateDirectionValidated: the column side of ORDER BY was
// checked against the aggregate aliases, the direction was concatenated raw.
// Safe from HTTP (the endpoint constrains it), reachable from ctx.Aggregate.
func TestAuditLows_AggregateDirectionValidated(t *testing.T) {
	t.Parallel()

	var injErr, okErr error
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.Post{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/posts/agg",
				Handler: func(ctx *maniflex.ServerContext) error {
					spec := func(dir maniflex.SortDir) maniflex.AggregateQuery {
						return maniflex.AggregateQuery{
							Select:  []maniflex.AggregateField{{Op: maniflex.AggCount, Field: "id", As: "n"}},
							GroupBy: []string{"user_id"},
							OrderBy: []maniflex.SortExpr{{DBName: "n", Direction: dir}},
						}
					}
					_, injErr = ctx.Aggregate("Post", spec("asc; DROP TABLE posts--"))
					_, okErr = ctx.Aggregate("Post", spec(maniflex.SortDesc))
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})
	srv.GET("/posts/agg").AssertStatus(http.StatusOK)

	if injErr == nil {
		t.Error("a non-asc/desc direction must be refused, not concatenated into the SQL")
	}
	if okErr != nil {
		t.Errorf("a legitimate desc must still work: %v", okErr)
	}
}

// ── MS-L5: deterministic locale fallback ─────────────────────────────────────

// TestAuditLows_LocaleFallbackIsStable: the last-resort branch ranged the map
// and returned whatever came first, which Go randomises. The field therefore
// changed between identical reads, and the response feeds the ETag — so an
// unchanged row produced a new ETag each time and optimistic-lock writes failed
// with spurious 412s.
func TestAuditLows_LocaleFallbackIsStable(t *testing.T) {
	t.Parallel()

	// A bare split-mode server: splitServer registers validate.RequireLocale,
	// which would reject a name carrying none of the chain's locales — which is
	// exactly the shape this test needs.
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{SplitDept{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
				Supported: []string{"en", "ar"}, Default: "en", FromHeader: true,
			}))
		},
	})
	// None of these locales is in the request chain (en/ar), so resolution falls
	// all the way through to the last resort.
	id := srv.MustID(srv.POST("/split_depts", map[string]any{
		"name": map[string]any{"fr": "Cardiologie", "de": "Kardiologie", "es": "Cardiologia"},
		"code": "CARD",
	}))

	first := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()["name"]
	for i := 0; i < 20; i++ {
		got := srv.GET("/split_depts/" + id).AssertStatus(http.StatusOK).Data()["name"]
		if got != first {
			t.Fatalf("read %d returned %#v, first read returned %#v — fallback is not deterministic",
				i, got, first)
		}
	}
	// Lexicographically smallest key wins, so the choice is predictable rather
	// than merely stable within one process.
	if first != "Kardiologie" {
		t.Errorf("fallback should pick the smallest key (de), got %#v", first)
	}
}
