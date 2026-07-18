package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	mdb "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Models ───────────────────────────────────────────────────────────────────

// rsDoc soft-deletes with a deleted_at timestamp and opts into the restore
// route.
type rsDoc struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title  string `json:"title" db:"title" mfx:"required"`
	Tenant string `json:"tenant" db:"tenant" mfx:"filterable"`
}

// rsFlag soft-deletes with a bool marker — the other style, which needs the
// opposite SQL in both directions.
type rsFlag struct {
	maniflex.BaseModel
	maniflex.WithIsDeleted
	Title string `json:"title" db:"title" mfx:"required"`
}

// rsNoOptIn soft-deletes but never opts in: the route must not exist.
type rsNoOptIn struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title string `json:"title" db:"title" mfx:"required"`
}

// rsHard opts in but hard-deletes, so there is nothing to restore and the route
// must not be mounted.
type rsHard struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
}

func restoreServer(t *testing.T, extra func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			rsDoc{}, maniflex.ModelConfig{RestoreEnabled: true},
			rsFlag{}, maniflex.ModelConfig{RestoreEnabled: true},
			rsNoOptIn{}, maniflex.ModelConfig{},
			rsHard{}, maniflex.ModelConfig{RestoreEnabled: true},
		},
		Middleware: extra,
	})
}

// TestRestore covers roadmap item 5.19 — the soft-delete restore endpoint.
//
//	go test ./tests/e2e/... -run TestRestore
func TestRestore(t *testing.T) {
	t.Parallel()

	t.Run("restores_a_soft_deleted_row", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		id := srv.MustID(srv.POST("/rs_docs", map[string]any{"title": "Draft"}))

		srv.DELETE("/rs_docs/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/rs_docs/" + id).AssertStatus(http.StatusNotFound)

		resp := srv.POST("/rs_docs/"+id+"/restore", nil)
		resp.AssertStatus(http.StatusOK)
		// The restored record comes back, so a client need not re-read it.
		if got := testutil.Field(t, resp.Data(), "title"); got != "Draft" {
			t.Errorf("restored title: got %v want Draft", got)
		}

		// And it is genuinely live again — readable and listable.
		srv.GET("/rs_docs/" + id).AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "rs_docs after restore", srv.GET("/rs_docs").DataList(), 1)
	})

	t.Run("restoring_a_live_row_is_404", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		id := srv.MustID(srv.POST("/rs_docs", map[string]any{"title": "Live"}))

		// Mirrors the re-delete guard: a no-op the caller should hear about
		// rather than a silent success.
		srv.POST("/rs_docs/"+id+"/restore", nil).AssertStatus(http.StatusNotFound)
	})

	t.Run("restoring_an_unknown_id_is_404", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		srv.POST("/rs_docs/00000000-0000-0000-0000-000000000000/restore", nil).
			AssertStatus(http.StatusNotFound)
	})

	t.Run("bool_marker_style_restores", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		id := srv.MustID(srv.POST("/rs_flags", map[string]any{"title": "Flagged"}))

		srv.DELETE("/rs_flags/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/rs_flags/" + id).AssertStatus(http.StatusNotFound)
		srv.POST("/rs_flags/"+id+"/restore", nil).AssertStatus(http.StatusOK)
		srv.GET("/rs_flags/" + id).AssertStatus(http.StatusOK)
	})

	t.Run("route_absent_without_opt_in", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		id := srv.MustID(srv.POST("/rs_no_opt_ins", map[string]any{"title": "X"}))
		srv.DELETE("/rs_no_opt_ins/" + id).AssertStatus(http.StatusNoContent)

		// Un-deleting is privileged; it must not appear merely because the model
		// soft-deletes.
		srv.POST("/rs_no_opt_ins/"+id+"/restore", nil).AssertStatus(http.StatusNotFound)
	})

	t.Run("route_absent_for_hard_delete_model", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		id := srv.MustID(srv.POST("/rs_hards", map[string]any{"title": "Y"}))
		srv.DELETE("/rs_hards/" + id).AssertStatus(http.StatusNoContent)

		// Opted in, but the row is gone — there is nothing a restore could do.
		srv.POST("/rs_hards/"+id+"/restore", nil).AssertStatus(http.StatusNotFound)
	})

	t.Run("restore_does_not_bump_updated_at", func(t *testing.T) {
		t.Parallel()
		srv := restoreServer(t, nil)
		created := srv.POST("/rs_docs", map[string]any{"title": "Stamped"})
		id := srv.MustID(created)
		before := testutil.Field(t, created.Data(), "updated_at")

		srv.DELETE("/rs_docs/" + id).AssertStatus(http.StatusNoContent)
		time.Sleep(1100 * time.Millisecond) // so a bump would be visible
		restored := srv.POST("/rs_docs/"+id+"/restore", nil)
		restored.AssertStatus(http.StatusOK)

		// A restore is not an edit: anything watching updated_at must not be
		// told the content changed.
		if after := testutil.Field(t, restored.Data(), "updated_at"); after != before {
			t.Errorf("updated_at changed on restore: %v → %v", before, after)
		}
	})

	// The security case. A restore cannot read its target back through the
	// normal path to check scope — the row is soft-deleted and invisible to
	// every read — so the scope is pushed into the restore statement instead.
	// If that were dropped, any caller could un-delete another tenant's row by
	// knowing its id.
	t.Run("forced_filter_scopes_the_restore", func(t *testing.T) {
		t.Parallel()
		tenantOf := func(ctx *maniflex.ServerContext) any {
			if h := ctx.Request.Header.Get("X-Tenant"); h != "" {
				return h
			}
			return nil
		}
		srv := restoreServer(t, func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(mdb.ForceFilter("tenant", tenantOf),
				maniflex.ForModel("rsDoc"))
		})
		acme := map[string]string{"X-Tenant": "acme"}
		other := map[string]string{"X-Tenant": "other"}

		id := srv.MustID(srv.POST("/rs_docs", map[string]any{"title": "Acme's", "tenant": "acme"}, acme))
		srv.DELETE("/rs_docs/"+id, acme).AssertStatus(http.StatusNoContent)

		// Another tenant knows the id and asks for it back: refused.
		srv.POST("/rs_docs/"+id+"/restore", nil, other).AssertStatus(http.StatusNotFound)
		// Still deleted, for its owner too — the refusal did not restore it.
		srv.GET("/rs_docs/"+id, acme).AssertStatus(http.StatusNotFound)

		// The owner can.
		srv.POST("/rs_docs/"+id+"/restore", nil, acme).AssertStatus(http.StatusOK)
		srv.GET("/rs_docs/"+id, acme).AssertStatus(http.StatusOK)
	})

	// Restore dispatches as OpUpdate precisely so that an app's existing
	// "who may modify this row" middleware governs it without being rewritten.
	t.Run("update_middleware_governs_restore", func(t *testing.T) {
		t.Parallel()
		var sawUpdate, sawRestoreFlag int
		srv := restoreServer(t, func(s *maniflex.Server) {
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				sawUpdate++
				if ctx.IsRestore() {
					sawRestoreFlag++
				}
				return next()
			}, maniflex.ForOperation(maniflex.OpUpdate),
				maniflex.ForModel("rsDoc"),
				maniflex.AtPosition(maniflex.Before))
		})

		id := srv.MustID(srv.POST("/rs_docs", map[string]any{"title": "Guarded"}))
		srv.DELETE("/rs_docs/" + id).AssertStatus(http.StatusNoContent)
		srv.POST("/rs_docs/"+id+"/restore", nil).AssertStatus(http.StatusOK)

		if sawUpdate != 1 {
			t.Errorf("OpUpdate middleware ran %d times on a restore, want 1", sawUpdate)
		}
		// ...and can still tell the two apart when it needs to.
		if sawRestoreFlag != 1 {
			t.Errorf("ctx.IsRestore() was true %d times, want 1", sawRestoreFlag)
		}

		// A genuine update runs the same middleware, with the flag clear.
		srv.PATCH("/rs_docs/"+id, map[string]any{"title": "Edited"}).AssertStatus(http.StatusOK)
		if sawUpdate != 2 || sawRestoreFlag != 1 {
			t.Errorf("after a real update: runs=%d restoreFlag=%d, want 2 and 1", sawUpdate, sawRestoreFlag)
		}
	})
}
