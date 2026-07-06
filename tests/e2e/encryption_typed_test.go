package e2e_test

// Regression (delivery svc #1): mfx:"encrypted" was applied only by the HTTP
// pipeline DB step. Typed / background access (maniflex.Create/Read and
// ctx.GetModel) went straight to the adapter and bypassed both encryption on
// write (plaintext at rest) and decryption on read. Both paths must now
// encrypt/decrypt like the HTTP path.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestEncryption_TypedAndAccessorRoundTrip(t *testing.T) {
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{Patient{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/patient-typed",
				Handler: func(ctx *maniflex.ServerContext) error {
					fail := func(code, msg string) { ctx.Abort(http.StatusInternalServerError, code, msg) }

					// ── Typed Create must encrypt at rest and return plaintext ──
					created, err := maniflex.Create(ctx, &Patient{NationalID: "999-88-7777", DOB: "2000-02-02", Name: "Zed"})
					if err != nil {
						return err
					}
					if created.NationalID != "999-88-7777" {
						fail("TYPED_CREATE", "typed Create should echo plaintext")
						return nil
					}
					id := created.ID

					stored, err := ctx.RawQuery("SELECT national_id FROM patients WHERE id = ?", id)
					if err != nil {
						return err
					}
					if s, _ := stored[0]["national_id"].(string); !strings.HasPrefix(s, "enc:") {
						fail("TYPED_CIPHERTEXT", "typed Create stored plaintext: "+s)
						return nil
					}

					// ── Typed Read must decrypt ──
					got, err := maniflex.Read[Patient](ctx, id)
					if err != nil {
						return err
					}
					if got.NationalID != "999-88-7777" {
						fail("TYPED_READ", "typed Read did not decrypt: "+got.NationalID)
						return nil
					}

					// ── ModelAccessor (ctx.GetModel) must encrypt + decrypt too ──
					m, err := ctx.GetModel("Patient").Create(map[string]any{
						"national_id": "111-22-3333", "dob": "1980-01-01", "name": "Acc",
					})
					if err != nil {
						return err
					}
					if m["national_id"] != "111-22-3333" {
						fail("ACC_CREATE", "accessor Create should echo plaintext")
						return nil
					}
					accID := m["id"].(string)
					accStored, err := ctx.RawQuery("SELECT national_id FROM patients WHERE id = ?", accID)
					if err != nil {
						return err
					}
					if s, _ := accStored[0]["national_id"].(string); !strings.HasPrefix(s, "enc:") {
						fail("ACC_CIPHERTEXT", "accessor Create stored plaintext")
						return nil
					}
					accRead, err := ctx.GetModel("Patient").Read(accID)
					if err != nil {
						return err
					}
					if accRead["national_id"] != "111-22-3333" {
						fail("ACC_READ", "accessor Read did not decrypt")
						return nil
					}

					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK}
					return nil
				},
			})
		},
	})

	srv.POST("/patient-typed", nil).AssertStatus(http.StatusOK)
}
