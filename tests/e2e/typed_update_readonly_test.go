package e2e

// Audit MS-5: maniflex.Update[T] is a full-record write, which is documented and
// pinned by TestTypedUpdate_FullRecordOverwritesOmittedFields. But "every column
// except id" also swept in the framework-managed, mfx:"readonly" created_at — so
// a typed update stamped the struct's zero time (0001-01-01) over the row's real
// creation date. readonly is enforced by the Validate step, which a typed helper
// never runs, and the adapter's buildUpdateSQL skips only id and updated_at.
//
//	go test ./tests/e2e/... -run TestTypedUpdateReadonly

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// tuReadonly separates the two kinds of protected column from an ordinary one,
// so a fix that over-reaches (freezing Name) is as visible as one that
// under-reaches (still clobbering CreatedAt).
type tuReadonly struct {
	maniflex.BaseModel
	Name   string `json:"name"   db:"name"`
	Note   string `json:"note"   db:"note"`
	Serial string `json:"serial" db:"serial" mfx:"immutable"`
	Origin string `json:"origin" db:"origin" mfx:"readonly"`
}

// The headline defect: a typed update must not destroy created_at.
func TestTypedUpdateReadonly_PreservesCreatedAt(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{tuReadonly{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/tu-created-at",
				Handler: func(ctx *maniflex.ServerContext) error {
					created, err := maniflex.Create(ctx, &tuReadonly{Name: "orig"})
					if err != nil {
						return err
					}
					if created.CreatedAt.IsZero() {
						ctx.Abort(http.StatusInternalServerError, "SETUP",
							"created_at was not stamped on create")
						return nil
					}

					// The realistic call: a caller builds a fresh struct with the
					// fields they mean to change. CreatedAt is left at the zero
					// time, exactly as it would be in any real caller.
					if _, err := maniflex.Update(ctx, created.ID,
						&tuReadonly{Name: "renamed"}); err != nil {
						return err
					}

					got, err := maniflex.Read[tuReadonly](ctx, created.ID)
					if err != nil {
						return err
					}
					if got.Name != "renamed" {
						ctx.Abort(http.StatusInternalServerError, "NAME", "name not updated")
						return nil
					}
					if !got.CreatedAt.Equal(created.CreatedAt) {
						ctx.Abort(http.StatusInternalServerError, "CREATED_AT",
							"created_at was overwritten: was "+created.CreatedAt.String()+
								", now "+got.CreatedAt.String())
						return nil
					}
					if got.CreatedAt.Year() <= 1 {
						ctx.Abort(http.StatusInternalServerError, "CREATED_AT_ZERO",
							"created_at was stamped with the struct's zero time")
						return nil
					}
					return nil
				},
			})
		},
	})

	srv.POST("/tu-created-at", map[string]any{}).AssertStatus(http.StatusOK)
}

// updated_at must still advance — it is framework-managed too, but managed by
// being rewritten, not by being left alone. A fix that lumps it in with
// created_at would freeze it.
func TestTypedUpdateReadonly_StillAdvancesUpdatedAt(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{tuReadonly{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/tu-updated-at",
				Handler: func(ctx *maniflex.ServerContext) error {
					created, err := maniflex.Create(ctx, &tuReadonly{Name: "orig"})
					if err != nil {
						return err
					}
					time.Sleep(5 * time.Millisecond)
					if _, err := maniflex.Update(ctx, created.ID,
						&tuReadonly{Name: "renamed"}); err != nil {
						return err
					}
					got, err := maniflex.Read[tuReadonly](ctx, created.ID)
					if err != nil {
						return err
					}
					if !got.UpdatedAt.After(created.UpdatedAt) {
						ctx.Abort(http.StatusInternalServerError, "UPDATED_AT",
							"updated_at did not advance: was "+created.UpdatedAt.String()+
								", now "+got.UpdatedAt.String())
						return nil
					}
					return nil
				},
			})
		},
	})

	srv.POST("/tu-updated-at", map[string]any{}).AssertStatus(http.StatusOK)
}

// The fix must cut in exactly one place, so this asserts both directions from a
// single update. An ordinary column left unset is still blanked — Update[T] is a
// documented full replace, and a fix that merely stopped writing unset fields
// would turn the helper into a PATCH while passing every test above. The
// readonly and immutable columns are preserved.
//
// Without both halves the test is vacuous: an earlier version of it checked only
// that the field it set came back changed, which was true before the fix as well.
func TestTypedUpdateReadonly_CutsInExactlyOnePlace(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{tuReadonly{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/tu-full-replace",
				Handler: func(ctx *maniflex.ServerContext) error {
					created, err := maniflex.Create(ctx, &tuReadonly{
						Name: "orig", Note: "keep-me?", Serial: "SN-1", Origin: "factory",
					})
					if err != nil {
						return err
					}
					if created.Serial != "SN-1" || created.Origin != "factory" {
						ctx.Abort(http.StatusInternalServerError, "SETUP",
							"immutable/readonly columns must still be writable on create")
						return nil
					}

					// Only Name is set; Note, Serial and Origin are left zero.
					if _, err := maniflex.Update(ctx, created.ID,
						&tuReadonly{Name: "renamed"}); err != nil {
						return err
					}
					got, err := maniflex.Read[tuReadonly](ctx, created.ID)
					if err != nil {
						return err
					}

					switch {
					case got.Name != "renamed":
						ctx.Abort(http.StatusInternalServerError, "NAME",
							"the field that was set did not change")
					case got.Note != "":
						ctx.Abort(http.StatusInternalServerError, "NOTE",
							"an ordinary unset column must still be blanked — Update[T] is a "+
								"full replace, not a PATCH; got "+got.Note)
					case got.Serial != "SN-1":
						ctx.Abort(http.StatusInternalServerError, "SERIAL",
							"an immutable column must not be overwritten; got "+got.Serial)
					case got.Origin != "factory":
						ctx.Abort(http.StatusInternalServerError, "ORIGIN",
							"a readonly column must not be overwritten; got "+got.Origin)
					}
					return nil
				},
			})
		},
	})

	srv.POST("/tu-full-replace", map[string]any{}).AssertStatus(http.StatusOK)
}
