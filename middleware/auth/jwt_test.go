package auth_test

import (
	"crypto/rsa"
	"testing"

	"github.com/xaleel/maniflex/middleware/auth"
)

// SEC-7: an empty HMAC secret provides no security (any token can be forged), so
// JWTAuth must refuse it at construction rather than silently accepting it.
func TestJWTAuth_EmptyHMACSecretPanics(t *testing.T) {
	assertPanics(t, `JWTAuth("")`, func() { auth.JWTAuth("") })
}

// SEC-7: the secret guard applies only to the HMAC path. Short secrets are
// permitted (warn only), and the asymmetric path (PublicKey set) ignores the
// secret entirely, so an empty secret there must not panic.
func TestJWTAuth_SecretGuardExemptions(t *testing.T) {
	assertNoPanic(t, "short secret (warns, no panic)", func() {
		auth.JWTAuth("short-secret")
	})
	assertNoPanic(t, "strong secret", func() {
		auth.JWTAuth("this-secret-is-thirty-two-bytes!!")
	})
	assertNoPanic(t, "empty secret + PublicKey (asymmetric)", func() {
		auth.JWTAuth("", auth.JWTOptions{PublicKey: &rsa.PublicKey{}})
	})
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

func assertNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: unexpected panic: %v", name, r)
		}
	}()
	fn()
}
