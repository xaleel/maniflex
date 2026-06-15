package e2e

// Write-path branch coverage. The DB step sources write columns from the typed
// record when its present-set matches the body (recordSourcedWrite), and
// otherwise falls back to toDBMap(ctx.ParsedBody).
//
// ctx.ParsedBody is now a *maniflex.RequestBody, not a bare map: the only way to
// mutate the body is ctx.SetField / ctx.DeleteField, which also sync the typed
// record. A raw `ctx.ParsedBody["k"] = v` is a COMPILE ERROR. That structurally
// eliminates the former bug where a raw value-overwrite (bypassing SetField) was
// silently dropped — the body and record can no longer drift for a present key,
// so SetField persists both injected and overwritten values.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// A middleware injects a field that was ABSENT from the request body via
// SetField; it must persist (write path picks it up from the synced record).
func TestWritePath_SetFieldInjectsAbsentKey(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.SetField("qty", 42) // qty was not in the body
					return next()
				},
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})
	resp := srv.POST("/widgets", map[string]any{"name": "x"}) // qty omitted
	resp.AssertStatus(http.StatusCreated)
	if got := srv.GET("/widgets/" + resp.ID()).Data()["qty"]; got != float64(42) {
		t.Errorf("injected qty = %v, want 42 (SetField on an absent key must persist)", got)
	}
}

// A middleware OVERWRITES a field already present in the body via SetField. This
// is the scenario that used to silently drop the write when middleware mutated
// ctx.ParsedBody directly; with the read-only RequestBody it must persist.
func TestWritePath_SetFieldOverwritesExistingKey(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{widget{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.SetField("qty", 42) // overwrite the body's qty=1
					return next()
				},
				maniflex.ForModel("widget"), maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})
	resp := srv.POST("/widgets", map[string]any{"name": "x", "qty": 1})
	resp.AssertStatus(http.StatusCreated)
	if got := srv.GET("/widgets/" + resp.ID()).Data()["qty"]; got != float64(42) {
		t.Errorf("overwritten qty = %v, want 42 (SetField overwrite must not be lost)", got)
	}
}

// A JSON key with no matching model field is ignored: no error, not persisted,
// absent from the response.
func TestWritePath_UnknownBodyKeyIgnored(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{widget{}}})
	resp := srv.POST("/widgets", map[string]any{"name": "x", "qty": 2, "bogus": "zzz"})
	resp.AssertStatus(http.StatusCreated)
	data := srv.GET("/widgets/" + resp.ID()).Data()
	if _, ok := data["bogus"]; ok {
		t.Errorf("unknown key 'bogus' leaked into the record: %v", data)
	}
	if got := data["qty"]; got != float64(2) {
		t.Errorf("qty = %v, want 2 (known fields persist alongside the ignored unknown)", got)
	}
}
