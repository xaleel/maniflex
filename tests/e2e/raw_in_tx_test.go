package e2e

// ctx.RawQuery/RawExec route through ctx.Tx when a transaction is active. When
// the Tx implementation cannot run raw SQL they used to fall through to the bare
// adapter — a different connection, outside the transaction — so a raw write the
// caller believed was atomic committed on its own and survived the rollback.
// Refuse the statement instead (BUG-12).

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// plainTxAdapter hands out transactions with no raw-SQL support — the shape of a
// third-party adapter that implements maniflex.Tx and nothing more.
type plainTxAdapter struct {
	maniflex.DBAdapter
}

func (a plainTxAdapter) BeginTx(ctx context.Context, opts *maniflex.TxOptions) (maniflex.Tx, error) {
	tx, err := a.DBAdapter.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return plainTx{Tx: tx}, nil
}

// plainTx embeds the Tx interface, so it promotes exactly the methods Tx declares
// — and no RawQueryContext/RawExecContext.
type plainTx struct {
	maniflex.Tx
}

// rawInTxServer registers an action that runs a raw write inside a transaction it
// opens itself, and reports what RawExec returned.
func rawInTxServer(t *testing.T, opts testutil.Options) *testutil.Server {
	t.Helper()
	opts.Models = []any{widget{}}
	opts.Middleware = func(s *maniflex.Server) {
		s.Action(maniflex.ActionConfig{
			Method: "POST",
			Path:   "/raw-in-tx",
			Handler: func(ctx *maniflex.ServerContext) error {
				w, err := maniflex.Create(ctx, &widget{Name: "before", Qty: 1})
				if err != nil {
					return err
				}

				tx, err := ctx.BeginTx(ctx.Ctx, nil)
				if err != nil {
					return err
				}
				ctx.Tx = tx
				defer tx.Rollback() //nolint:errcheck // rolled back deliberately below

				_, rawErr := ctx.RawExec("UPDATE widgets SET name = ? WHERE id = ?", "raw-write", w.ID)

				// Roll the transaction back. A raw write that participated in it
				// disappears; one that escaped onto the bare adapter persists.
				_ = tx.Rollback()
				ctx.Tx = nil

				code := "OK"
				if rawErr != nil {
					code = "RAW_REFUSED"
					if !errors.Is(rawErr, maniflex.ErrRawNotSupportedInTx) {
						code = "RAW_OTHER_ERROR"
					}
				}
				ctx.Response = &maniflex.APIResponse{
					StatusCode: http.StatusOK,
					Data:       map[string]any{"raw": code, "id": w.ID},
				}
				return nil
			},
		})
	}
	return testutil.NewServer(t, opts)
}

// A Tx that cannot run raw SQL must refuse the statement, not run it outside the
// transaction. The record keeps its pre-transaction value either way — but under
// the old fall-through it kept the raw write instead, despite the rollback.
func TestRawInTx_UnsupportedTxIsRefused(t *testing.T) {
	t.Parallel()
	srv := rawInTxServer(t, testutil.Options{
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			inner, err := sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			return plainTxAdapter{DBAdapter: inner}, nil
		},
	})

	data := srv.POST("/raw-in-tx", nil).AssertStatus(http.StatusOK).Data()
	if got := data["raw"]; got != "RAW_REFUSED" {
		t.Errorf("RawExec returned %v, want ErrRawNotSupportedInTx", got)
	}

	// The escaped write must not have landed.
	name := srv.GET("/widgets/" + data["id"].(string)).Data()["name"]
	if name != "before" {
		t.Errorf("name = %v, want \"before\" — the raw write ran outside the transaction and survived the rollback", name)
	}
}

// Config.Trace wraps the transaction in maniflex's own tracedTx, which embeds the
// Tx interface and so does not inherit raw support. Turning tracing on must not
// change where a raw statement runs.
func TestRawInTx_TracedTxStillParticipates(t *testing.T) {
	t.Parallel()
	srv := rawInTxServer(t, testutil.Options{
		Trace: maniflex.PipelineTrace{Enabled: true},
	})

	data := srv.POST("/raw-in-tx", nil).AssertStatus(http.StatusOK).Data()
	if got := data["raw"]; got != "OK" {
		t.Errorf("RawExec returned %v, want success — a traced tx must still run raw SQL", got)
	}

	// It ran inside the transaction, so the rollback undid it.
	name := srv.GET("/widgets/" + data["id"].(string)).Data()["name"]
	if name != "before" {
		t.Errorf("name = %v, want \"before\" — the raw write escaped the traced transaction and survived the rollback", name)
	}
}

// The ordinary path is unchanged: a tx that supports raw SQL runs it inside the
// transaction, and the rollback takes it with it.
func TestRawInTx_SupportedTxParticipates(t *testing.T) {
	t.Parallel()
	srv := rawInTxServer(t, testutil.Options{})

	data := srv.POST("/raw-in-tx", nil).AssertStatus(http.StatusOK).Data()
	if got := data["raw"]; got != "OK" {
		t.Errorf("RawExec returned %v, want success", got)
	}
	name := srv.GET("/widgets/" + data["id"].(string)).Data()["name"]
	if name != "before" {
		t.Errorf("name = %v, want \"before\" — the raw write survived the rollback", name)
	}
}
