package e2e

// P0 #6 — soft-delete must filter typed reads INSIDE a transaction. The tx read
// path scans via scanStruct (unified with the pooled adapter as of Phase 6), so
// the soft-delete WHERE clause must apply there too. A rolled-back delete must
// also leave the row live.

import (
	"errors"
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

type sdNote struct {
	maniflex.BaseModel
	maniflex.WithDeletedAt
	Title string `json:"title" db:"title" mfx:"required"`
}

func TestSoftDelete_InsideTransaction(t *testing.T) {
	var inTxCount = -1 // count seen by an in-transaction typed list after a delete

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{sdNote{}},
		Middleware: func(s *maniflex.Server) {
			// Commit path: create two notes, then inside a batch tx delete one and
			// list — the in-tx list must already exclude the soft-deleted row.
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/sd-commit",
				Handler: func(ctx *maniflex.ServerContext) error {
					a, err := maniflex.Create(ctx, &sdNote{Title: "a"})
					if err != nil {
						return err
					}
					if _, err := maniflex.Create(ctx, &sdNote{Title: "b"}); err != nil {
						return err
					}
					if err := maniflex.Batch(ctx, func(*maniflex.Batcher) error {
						if err := maniflex.Delete[sdNote](ctx, a.ID); err != nil {
							return err
						}
						items, err := maniflex.List[sdNote](ctx, nil)
						if err != nil {
							return err
						}
						inTxCount = len(items)
						return nil
					}); err != nil {
						return err
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
			// Rollback path: delete inside a batch that errors → the delete is
			// undone and the row stays live.
			s.Action(maniflex.ActionConfig{
				Method: "POST", Path: "/sd-rollback",
				Handler: func(ctx *maniflex.ServerContext) error {
					n, err := maniflex.Create(ctx, &sdNote{Title: "keep"})
					if err != nil {
						return err
					}
					_ = maniflex.Batch(ctx, func(*maniflex.Batcher) error {
						if err := maniflex.Delete[sdNote](ctx, n.ID); err != nil {
							return err
						}
						return errors.New("force rollback")
					})
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	// Commit path.
	srv.POST("/sd-commit", nil).AssertStatus(http.StatusOK)
	if inTxCount != 1 {
		t.Errorf("in-tx typed list after delete = %d, want 1 (soft-delete filter must apply on the tx read path)", inTxCount)
	}
	if live := srv.GET("/sd_notes").DataList(); len(live) != 1 {
		t.Errorf("after commit: live notes = %d, want 1 (one soft-deleted)", len(live))
	}

	// Rollback path: 'keep' must still be live.
	srv.POST("/sd-rollback", nil).AssertStatus(http.StatusOK)
	found := false
	for _, it := range srv.GET("/sd_notes").DataList() {
		if m, ok := it.(map[string]any); ok && m["title"] == "keep" {
			found = true
		}
	}
	if !found {
		t.Error("after rollback: 'keep' note should still be live (delete must be undone)")
	}
}
