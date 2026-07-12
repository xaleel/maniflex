package e2e

// The transactional soft-delete omitted the "not already deleted" guard the
// pooled adapter applies, so an UPDATE against an already-deleted row still
// reported one affected row: re-deleting inside a transaction re-stamped the
// column and answered 204, where the same request outside one answers 404
// (BUG-15).

import (
	"errors"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// sdFlagNote covers the boolean soft-delete column; sdNote (softdelete_tx_test.go)
// covers the timestamp one. The guard differs per column type.
type sdFlagNote struct {
	maniflex.BaseModel
	maniflex.WithIsDeleted
	Title string `json:"title" db:"title" mfx:"required"`
}

func TestSoftDelete_RedeleteInsideTransaction(t *testing.T) {
	t.Parallel()

	var typedRedelete error // error from a typed re-delete inside a batch tx

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{sdNote{}, sdFlagNote{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				maniflex.WithTransaction(nil),
				maniflex.ForOperation(maniflex.OpDelete),
			)
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/sd-redelete",
				Handler: func(ctx *maniflex.ServerContext) error {
					n, err := maniflex.Create(ctx, &sdNote{Title: "twice"})
					if err != nil {
						return err
					}
					if err := maniflex.Batch(ctx, func(*maniflex.Batcher) error {
						if err := maniflex.Delete[sdNote](ctx, n.ID); err != nil {
							return err
						}
						typedRedelete = maniflex.Delete[sdNote](ctx, n.ID)
						return nil
					}); err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	t.Run("deleted_at_column", func(t *testing.T) {
		id := srv.MustID(srv.POST("/sd_notes", map[string]any{"title": "a"}))
		srv.DELETE("/sd_notes/" + id).AssertStatus(http.StatusNoContent)
		srv.DELETE("/sd_notes/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("is_deleted_column", func(t *testing.T) {
		id := srv.MustID(srv.POST("/sd_flag_notes", map[string]any{"title": "b"}))
		srv.DELETE("/sd_flag_notes/" + id).AssertStatus(http.StatusNoContent)
		srv.DELETE("/sd_flag_notes/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("typed_delete_in_batch", func(t *testing.T) {
		srv.POST("/sd-redelete", nil).AssertStatus(http.StatusOK)
		if !errors.Is(typedRedelete, maniflex.ErrNotFound) {
			t.Errorf("re-delete inside a batch tx: got %v, want ErrNotFound", typedRedelete)
		}
	})
}
