package e2e_test

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type encDefDoc struct {
	maniflex.BaseModel
	Secret string `json:"secret" mfx:"encrypted"`
	Status string `json:"status" mfx:"default:active"`
}

// TestTypedCreate_DefaultsOnEncryptedModel covers the other write path. A model
// with encrypted fields goes through the map bridge rather than the struct
// fast-path, so a fix applied only where buildInsertSQL reads the present set
// would leave these models still writing every column at its zero.
func TestTypedCreate_DefaultsOnEncryptedModel(t *testing.T) {
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)

	var status, secret string
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{encDefDoc{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "POST",
				Path:   "/enc_def_docs/typed",
				Handler: func(ctx *maniflex.ServerContext) error {
					out, err := maniflex.Create(ctx, &encDefDoc{Secret: "shh"})
					if err != nil {
						return err
					}
					status, secret = out.Status, out.Secret
					ctx.Response = &maniflex.APIResponse{StatusCode: http.StatusOK, Data: map[string]any{}}
					return nil
				},
			})
		},
	})
	srv.POST("/enc_def_docs/typed", map[string]any{}).AssertStatus(http.StatusOK)

	if status != "active" {
		t.Errorf("encrypted model default: got status %q, want active", status)
	}
	// The present set must not have cost the encrypted field its round-trip.
	if secret != "shh" {
		t.Errorf("encrypted field: got %q, want shh", secret)
	}
}
