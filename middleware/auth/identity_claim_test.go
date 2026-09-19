package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// tokenWithClaims builds a compact token around a literal claims object and
// decodes the claims the way parseJWT does. The literal is what matters: it
// controls exactly how each number is written, which a map marshalled by
// encoding/json would not. The signature is irrelevant to identityClaim.
func tokenWithClaims(t *testing.T, claimsJSON string) (string, map[string]any) {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	token := enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(claimsJSON)) + ".sig"
	var claims map[string]any
	if err := json.Unmarshal([]byte(claimsJSON), &claims); err != nil {
		t.Fatalf("claims %s: %v", claimsJSON, err)
	}
	return token, claims
}

// Audit AUTH-5: the user id was read with a bare .(string) whose failure was
// discarded, so a numeric sub became "". The obvious repair — format the decoded
// float64 — is its own bug: float64 is exact only to 2^53, so neighbouring large
// ids would format identically and two users would silently become one.
func TestIdentityClaim(t *testing.T) {
	for _, tc := range []struct {
		name, claims, want string
		wantErr            string // substring; "" means no error
	}{
		{"string", `{"sub":"alice"}`, "alice", ""},
		{"integer", `{"sub":42}`, "42", ""},
		{"negative integer", `{"sub":-7}`, "-7", ""},
		{"exponent notation is the same integer", `{"sub":4.2e1}`, "42", ""},
		{"trailing .0 is the same integer", `{"sub":42.0}`, "42", ""},
		// The pair the float64 route merges. Both must survive, and differ.
		{"2^53", `{"sub":9007199254740992}`, "9007199254740992", ""},
		{"2^53+1", `{"sub":9007199254740993}`, "9007199254740993", ""},
		{"uint64 max", `{"sub":18446744073709551615}`, "18446744073709551615", ""},
		{"absent", `{"iss":"x"}`, "", ""},
		{"null", `{"sub":null}`, "", ""},
		{"fraction", `{"sub":4.5}`, "", "not an integer"},
		{"boolean", `{"sub":true}`, "", "boolean"},
		{"object", `{"sub":{"id":1}}`, "", "object"},
		{"array", `{"sub":[1]}`, "", "array"},
		{"absurdly long number", `{"sub":` + strings.Repeat("9", maxIdentityDigits+1) + `}`, "", "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, claims := tokenWithClaims(t, tc.claims)
			got, err := identityClaim(token, claims, "sub")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got (%q, %v), want an error mentioning %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
