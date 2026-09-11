package e2e

// The Response step used to panic on ctx.DBResult shapes a Replace middleware can
// plausibly produce: a ListResult with no Query (nil deref), one with a zero Limit
// (integer divide-by-zero in the page count), or a value that isn't a record at
// all (reflect.Value.Elem on a non-pointer). Each surfaced as a 500 PANIC on
// every request to the route (BUG-13).

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// replaceDBStep swaps the DB step for one that sets ctx.DBResult to whatever the
// test wants to hand the Response step.
func replaceDBStep(t *testing.T, op maniflex.Operation, result func() any) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.DBResult = result()
					return next()
				},
				maniflex.ForModel("widget"),
				maniflex.ForOperation(op),
				maniflex.AtPosition(maniflex.Replace),
			)
		},
	})
}

// The audit's scenario: a Replace list step that sets only Items. Query is nil,
// so the page count divided by a zero Limit.
func TestResponseGuard_ListResultWithoutQuery(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return &maniflex.ListResult{
			Items: []any{&widget{Name: "a", Qty: 1}},
			Total: 1,
		}
	})

	resp := srv.GET("/widgets").AssertStatus(http.StatusOK)
	if n := len(resp.DataList()); n != 1 {
		t.Fatalf("got %d items, want 1", n)
	}
	// The pagination meta is filled from defaults rather than exploding.
	meta := resp.Meta()
	if meta["limit"] != float64(20) {
		t.Errorf("limit = %v, want the default 20", meta["limit"])
	}
	if meta["page"] != float64(1) {
		t.Errorf("page = %v, want 1", meta["page"])
	}
	if meta["pages"] != float64(1) {
		t.Errorf("pages = %v, want 1", meta["pages"])
	}
}

// Same shape, but with a Query whose Limit is left at its zero value.
func TestResponseGuard_ListResultWithZeroLimit(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return &maniflex.ListResult{
			Items: []any{&widget{Name: "a", Qty: 1}, &widget{Name: "b", Qty: 2}},
			Total: 2,
			Query: &maniflex.QueryParams{}, // Limit and Page both zero
		}
	})

	resp := srv.GET("/widgets").AssertStatus(http.StatusOK)
	if n := len(resp.DataList()); n != 2 {
		t.Fatalf("got %d items, want 2", n)
	}
	if got := resp.Meta()["pages"]; got != float64(1) {
		t.Errorf("pages = %v, want 1", got)
	}
}

// A DBResult of the wrong shape entirely is a bug in the middleware, not a panic
// in the framework: report it as a 500 that says what was expected.
func TestResponseGuard_ListResultWrongType(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return map[string]any{"not": "a list result"}
	})

	resp := srv.GET("/widgets")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}

// The read path has the same hazard: marshalRecord reflects into the value, so a
// non-pointer DBResult panicked inside reflect.Value.Elem.
func TestResponseGuard_ReadResultNotARecord(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpRead, func() any {
		return widget{Name: "by-value", Qty: 1} // a value, not a *widget
	})

	resp := srv.GET("/widgets/some-id")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}

// ── Audit STEP-8: a pointer is not enough, it has to be this model's ─────────
//
// BUG-13's guard accepted any non-nil pointer, and marshalRecord walks the
// model's field indices through whatever struct it is handed. Where the shapes
// differed that panicked; where they lined up it silently served the other
// struct's fields under this model's names — and since the hidden check reads
// this model's fields, a hidden column of the other struct went out in the clear.

// vault has widget's layout exactly, so every index widget's fields use lands on
// a vault field — the case that did not panic but leaked.
type vault struct {
	maniflex.BaseModel
	PasswordHash string `json:"password_hash" mfx:"hidden"`
	Pin          int    `json:"pin"           mfx:"hidden"`
}

const vaultHash = "$2a$10$not-for-the-wire"

func assertRefusedWithoutLeak(t *testing.T, resp *testutil.Response) {
	t.Helper()
	// First, because it is the harm: the bug answered 200 with a body, and
	// ErrorCode below stops the test on a body with no error object.
	if strings.Contains(string(resp.Body), vaultHash) {
		t.Errorf("the other struct's hidden field reached the wire: %s", resp.Body)
	}
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}

func TestResponseGuard_ReadResultOfAnotherModelIsRefused(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpRead, func() any {
		return &vault{PasswordHash: vaultHash, Pin: 1234}
	})
	assertRefusedWithoutLeak(t, srv.GET("/widgets/some-id"))
}

// The list path renders each row through the same function, and the audit entry
// did not cover it.
func TestResponseGuard_ListRowOfAnotherModelIsRefused(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpList, func() any {
		return &maniflex.ListResult{
			Items: []any{&widget{Name: "fine"}, &vault{PasswordHash: vaultHash}},
			Total: 2,
		}
	})
	assertRefusedWithoutLeak(t, srv.GET("/widgets"))
}

// Export streams its rows after the headers are sent, so a bad row has to be
// caught before streaming starts or there is no clean 500 left to answer with.
func TestResponseGuard_ExportRowOfAnotherModelIsRefused(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}, maniflex.ModelConfig{ExportEnabled: true}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.DBResult = &maniflex.ListResult{Items: []any{&vault{PasswordHash: vaultHash}}, Total: 1}
					return next()
				},
				maniflex.ForModel("widget"),
				maniflex.ForOperation(maniflex.OpExport),
				maniflex.AtPosition(maniflex.Replace),
			)
		},
	})
	assertRefusedWithoutLeak(t, srv.GET("/widgets/export?format=csv"))
}

// The shapes that did panic, now named instead.
func TestResponseGuard_NonRecordPointersAreRefused(t *testing.T) {
	t.Parallel()
	n := 42
	for _, tc := range []struct {
		name string
		op   maniflex.Operation
		path string
		set  func() any
	}{
		{"pointer to an int", maniflex.OpRead, "/widgets/some-id", func() any { return &n }},
		{"a ListResult on a read", maniflex.OpRead, "/widgets/some-id", func() any { return &maniflex.ListResult{} }},
		{"a list row that is not a record", maniflex.OpList, "/widgets", func() any {
			return &maniflex.ListResult{Items: []any{42}, Total: 1}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := replaceDBStep(t, tc.op, tc.set).GET(tc.path)
			resp.AssertStatus(http.StatusInternalServerError)
			if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
				t.Errorf("error code: got %q, want INVALID_DB_RESULT — a panic is not "+
					"an answer that tells the developer what they set", code)
			}
		})
	}
}

// The contract is *T of the model, which is what pipeline.md documents. A type
// defined from the model shares its layout and would read correctly today, but
// only by that coincidence; admitting it would make the guard a layout check
// rather than a type check. Pinned so loosening it is a decision, not a drift.
type widgetView widget

func TestResponseGuard_LayoutIdenticalTypeIsRefused(t *testing.T) {
	t.Parallel()
	srv := replaceDBStep(t, maniflex.OpRead, func() any { return &widgetView{Name: "view"} })

	resp := srv.GET("/widgets/some-id")
	resp.AssertStatus(http.StatusInternalServerError)
	if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
		t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
	}
}

// A record operation whose Replace middleware set nothing used to be handed the
// list fallback and rendered as a record. The 5xx message stays in the log, so
// that is where to look for what the developer is told: that nothing was set,
// not that they set a ListResult.
func TestResponseGuard_RecordOpWithNoResultSaysNil(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   maniflex.Operation
		call func(*testutil.Server) *testutil.Response
	}{
		{maniflex.OpRead, func(s *testutil.Server) *testutil.Response { return s.GET("/widgets/some-id") }},
		{maniflex.OpCreate, func(s *testutil.Server) *testutil.Response {
			return s.POST("/widgets", map[string]any{"name": "a"})
		}},
		{maniflex.OpUpdate, func(s *testutil.Server) *testutil.Response {
			return s.PATCH("/widgets/some-id", map[string]any{"name": "a"})
		}},
	} {
		t.Run(string(tc.op), func(t *testing.T) {
			t.Parallel()
			logs := &lockedBuffer{}
			srv := testutil.NewServer(t, testutil.Options{
				Models: []any{widget{}},
				Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
				Middleware: func(s *maniflex.Server) {
					s.Pipeline.DB.Register(
						func(ctx *maniflex.ServerContext, next func() error) error { return next() },
						maniflex.ForModel("widget"),
						maniflex.ForOperation(tc.op),
						maniflex.AtPosition(maniflex.Replace),
					)
				},
			})

			resp := tc.call(srv)
			resp.AssertStatus(http.StatusInternalServerError)
			if code := resp.ErrorCode(); code != "INVALID_DB_RESULT" {
				t.Errorf("error code: got %q, want INVALID_DB_RESULT", code)
			}
			out := logs.String()
			if !strings.Contains(out, "got <nil>") {
				t.Errorf("the log does not say the result was never set:\n%s", out)
			}
			if strings.Contains(out, "ListResult") {
				t.Errorf("the log names a ListResult the middleware never set:\n%s", out)
			}
		})
	}
}

// ── What must keep working ──────────────────────────────────────────────────

// A list whose Replace middleware set nothing is still an empty page: that is an
// honest answer for a list, and the reason the fallback exists at all.
func TestResponseGuard_ListWithNoResultIsAnEmptyPage(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.DB.Register(
				func(ctx *maniflex.ServerContext, next func() error) error { return next() },
				maniflex.ForModel("widget"),
				maniflex.ForOperation(maniflex.OpList),
				maniflex.AtPosition(maniflex.Replace),
			)
		},
	})
	resp := srv.GET("/widgets").AssertStatus(http.StatusOK)
	if n := len(resp.DataList()); n != 0 {
		t.Errorf("got %d items, want an empty page", n)
	}
}

// The two shapes the contract names both still render.
func TestResponseGuard_ModelPointerAndMapStillRender(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func() any
	}{
		{"*widget", func() any { return &widget{Name: "typed", Qty: 3} }},
		{"map[string]any", func() any { return map[string]any{"name": "typed", "qty": 3} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := replaceDBStep(t, maniflex.OpRead, tc.set).
				GET("/widgets/some-id").AssertStatus(http.StatusOK).Data()
			if data["name"] != "typed" {
				t.Errorf("name = %#v, want %q", data["name"], "typed")
			}
		})
	}
}
