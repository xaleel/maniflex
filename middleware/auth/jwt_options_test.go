package auth

import (
	"strings"
	"testing"
	"time"
)

// Audit AUTH-8: ValidateProduction audits whether an access decision exists,
// never how strong the authenticator making it is — a middleware is an opaque
// closure to it. These options are therefore reported where they are set.

// A negative ClockSkew is not a stricter setting. The nbf and iat checks
// subtract it, so a negative value puts every freshly minted token in the
// future and authentication stops entirely, with a 401 that blames the token.
func TestJWTOptionsNegativeClockSkewPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a negative ClockSkew was accepted; it refuses every token carrying an iat")
		}
		msg, _ := r.(string)
		for _, want := range []string{"ClockSkew", "negative", "iat"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic does not mention %q: %s", want, msg)
			}
		}
	}()
	_ = JWTAuth("a-signing-secret-of-at-least-32-bytes", JWTOptions{ClockSkew: -time.Second})
}

func TestJWTOptionsClockSkewWarnings(t *testing.T) {
	const secret = "a-signing-secret-of-at-least-32-bytes"
	t.Run("a large tolerance warns", func(t *testing.T) {
		buf := captureDefaultLogger(t)
		_ = JWTAuth(secret, JWTOptions{ClockSkew: time.Hour})
		if !strings.Contains(buf.String(), "ClockSkew is large") {
			t.Errorf("no warning for an hour of skew; got %q", buf.String())
		}
	})
	// Must still work: the tolerance the option exists for draws nothing. A
	// warning on an ordinary setting is one nobody reads by the time it matters.
	t.Run("an ordinary tolerance is silent", func(t *testing.T) {
		for _, skew := range []time.Duration{0, 30 * time.Second, maxRecommendedClockSkew} {
			buf := captureDefaultLogger(t)
			_ = JWTAuth(secret, JWTOptions{ClockSkew: skew})
			if buf.Len() != 0 {
				t.Errorf("ClockSkew %s logged %q, want silence", skew, buf.String())
			}
		}
	})
}

// An identity provider signs for every application in its tenant with the same
// keys, so a verified signature does not mean the token was minted for us.
func TestJWKSAuthWarnsWithoutAudience(t *testing.T) {
	buf := captureDefaultLogger(t)
	_ = JWKSAuth("https://issuer.example.com/.well-known/jwks.json")
	out := buf.String()
	if !strings.Contains(out, "no Audience configured") {
		t.Fatalf("no audience warning; got %q", out)
	}
	if !strings.Contains(out, "other clients") {
		t.Errorf("the warning does not say what the risk is: %q", out)
	}
}

// Must still work: configured, it says nothing. Covered for the secure and
// loopback URLs by TestJWKSAuthSilentOnSecureAndLoopbackIssuers; asserted here
// against the audience option itself so the two cannot drift apart.
func TestJWKSAuthSilentWithAudience(t *testing.T) {
	buf := captureDefaultLogger(t)
	_ = JWKSAuth("https://issuer.example.com/.well-known/jwks.json", JWTOptions{Audience: "api"})
	if buf.Len() != 0 {
		t.Errorf("logged %q, want silence", buf.String())
	}
}
