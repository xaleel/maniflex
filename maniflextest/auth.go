package maniflextest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xaleel/maniflex"
)

const principalHeader = "X-Maniflex-Test-Principal"

// Human returns a test principal representing an interactive user.
func Human(userID string, roles ...string) maniflex.AuthInfo {
	return maniflex.AuthInfo{
		UserID:       userID,
		Roles:        append([]string(nil), roles...),
		Claims:       map[string]any{},
		IdentityType: maniflex.IdentityHuman,
		AuthMethod:   "test",
	}
}

// ServiceAccount returns a test principal representing a machine caller.
func ServiceAccount(userID string, scopes ...string) maniflex.AuthInfo {
	return maniflex.AuthInfo{
		UserID:       userID,
		Claims:       map[string]any{},
		IdentityType: maniflex.IdentityServiceAccount,
		Scopes:       append([]string(nil), scopes...),
		AuthMethod:   "test",
	}
}

// As injects principal through the harness's test-auth middleware.
func As(principal maniflex.AuthInfo) RequestOption {
	return func(req *http.Request) error {
		if principal.Claims == nil {
			principal.Claims = map[string]any{}
		}
		raw, err := json.Marshal(principal)
		if err != nil {
			return fmt.Errorf("encode test principal: %w", err)
		}
		req.Header.Set(principalHeader, base64.RawURLEncoding.EncodeToString(raw))
		return nil
	}
}

func testAuth(ctx *maniflex.ServerContext, next func() error) error {
	encoded := ctx.Request.Header.Get(principalHeader)
	if encoded == "" {
		return next()
	}

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		ctx.Abort(http.StatusBadRequest, "INVALID_TEST_PRINCIPAL", "invalid test principal")
		return nil
	}

	var principal maniflex.AuthInfo
	if err := json.Unmarshal(raw, &principal); err != nil {
		ctx.Abort(http.StatusBadRequest, "INVALID_TEST_PRINCIPAL", "invalid test principal")
		return nil
	}
	if principal.Claims == nil {
		principal.Claims = map[string]any{}
	}
	ctx.Auth = &principal
	return next()
}
