package bcrypt

// Audit AUTH-15: the cost was passed straight through to
// GenerateFromPassword, which checks it per call, and the 72-byte password
// limit surfaced as a generic error that HashField reported as a 500.
//
// Two silent failures came from the cost alone. Hasher(32) compiled and then
// answered 500 to every create and update, because nothing validated the number
// until a request arrived. Hasher(2) quietly became DefaultCost, so an operator
// who had deliberately chosen a number got a different one. Both are now
// panics at construction.
//
// This file is also the package's first test. It had none.

import (
	"errors"
	"strings"
	"testing"

	xbcrypt "golang.org/x/crypto/bcrypt"

	"github.com/xaleel/maniflex/middleware/service"
)

func TestHasher_AcceptsTheAllowedCostRange(t *testing.T) {
	t.Parallel()
	// MinCost and MaxCost themselves must be accepted: an off-by-one here would
	// panic on a legal configuration. MaxCost is not exercised with a real hash
	// because cost 31 takes minutes.
	for _, cost := range []int{xbcrypt.MinCost, xbcrypt.DefaultCost, xbcrypt.MaxCost} {
		if h := Hasher(cost); h == nil {
			t.Errorf("cost %d: Hasher returned nil", cost)
		}
	}

	// The default, and a cheap explicit cost, actually hash.
	for name, h := range map[string]service.Hasher{
		"default":   Hasher(),
		"min cost":  Hasher(xbcrypt.MinCost),
		"cost four": Hasher(4),
	} {
		out, err := h("a-real-password")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.HasPrefix(out, "$2") {
			t.Errorf("%s: not a bcrypt hash: %q", name, out)
		}
		if err := xbcrypt.CompareHashAndPassword([]byte(out), []byte("a-real-password")); err != nil {
			t.Errorf("%s: the hash does not verify against its own input: %v", name, err)
		}
	}
}

func TestHasher_PanicsOnACostOutsideTheRange(t *testing.T) {
	t.Parallel()
	for _, cost := range []int{
		xbcrypt.MinCost - 1, // silently became DefaultCost
		0,
		2,
		xbcrypt.MaxCost + 1, // used to 500 on every request
		64,
		-1,
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil {
					t.Errorf("cost %d must panic at construction", cost)
					return
				}
				// The message has to name the range, or the operator is left
				// guessing which way they were wrong.
				if msg, _ := r.(string); !strings.Contains(msg, "4..31") {
					t.Errorf("cost %d: panic does not name the allowed range: %v", cost, r)
				}
			}()
			Hasher(cost)
		}()
	}
}

// THE SECURITY-ADJACENT ONE: a password over the 72-BYTE limit is the caller's
// problem, so it is marked as client input for HashField to answer 400. The
// limit is in bytes and mfx:"maxlen:" counts characters, so no tag on the model
// can keep this path unreachable.
func TestHasher_OverLongPasswordIsMarkedAsClientInput(t *testing.T) {
	t.Parallel()
	h := Hasher(xbcrypt.MinCost)

	for _, tc := range []struct {
		name  string
		pw    string
		bytes int
	}{
		{"73 ascii bytes", strings.Repeat("x", 73), 73},
		// 25 characters, 100 bytes. An ordinary-length CJK or emoji passphrase,
		// and the reason maxlen:72 cannot close this.
		{"25 four-byte runes", strings.Repeat("\U0001F600", 25), 100},
		{"25 three-byte runes", strings.Repeat("日", 25), 75},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.pw) != tc.bytes {
				t.Fatalf("test setup: %d bytes, expected %d", len(tc.pw), tc.bytes)
			}
			_, err := h(tc.pw)
			if err == nil {
				t.Fatal("expected an error for a password over 72 bytes")
			}
			if !errors.Is(err, service.ErrClientInput) {
				t.Errorf("not marked as client input, so HashField would answer 500: %v", err)
			}
			// The cause survives for the message HashField builds.
			if !errors.Is(err, xbcrypt.ErrPasswordTooLong) {
				t.Errorf("the underlying cause was lost: %v", err)
			}
		})
	}
}

// MUST STILL WORK: exactly 72 bytes is allowed, so the boundary did not move.
func TestHasher_SeventyTwoBytesIsStillAccepted(t *testing.T) {
	t.Parallel()
	out, err := Hasher(xbcrypt.MinCost)(strings.Repeat("x", 72))
	if err != nil {
		t.Fatalf("72 bytes must be accepted: %v", err)
	}
	if !strings.HasPrefix(out, "$2") {
		t.Errorf("not a bcrypt hash: %q", out)
	}
}

// MUST STILL WORK: an error that is NOT about the caller's value stays
// unmarked, so HashField still reports it as the server fault it is. Nothing in
// bcrypt produces one for a valid cost, so this asserts the classification is
// narrow rather than blanket: a short password is not an error at all.
func TestHasher_OrdinaryValuesProduceNoClientInputError(t *testing.T) {
	t.Parallel()
	h := Hasher(xbcrypt.MinCost)
	for _, pw := range []string{"x", "short", strings.Repeat("y", 71)} {
		if _, err := h(pw); err != nil {
			t.Errorf("%q: unexpected error %v", pw, err)
		}
	}
}
