package maniflex_test

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// SEC-8: RandomString is a cryptographically-secure helper. Verify its
// observable contract: correct length, characters confined to the charset,
// sane edge cases, and non-colliding output.
func TestRandomString(t *testing.T) {
	// Length is honoured across a range of sizes.
	for _, n := range []int{1, 6, 32, 128} {
		if got := maniflex.RandomString(n, maniflex.ALPHANUM); len(got) != n {
			t.Errorf("RandomString(%d): length = %d, want %d", n, len(got), n)
		}
	}

	// Every character is drawn from the charset.
	const charset = maniflex.UPPER_D
	s := maniflex.RandomString(2000, charset)
	for i, r := range s {
		if !strings.ContainsRune(charset, r) {
			t.Fatalf("char %d = %q not in charset %q", i, r, charset)
		}
	}

	// Edge cases: non-positive length or empty charset yield "".
	if got := maniflex.RandomString(0, maniflex.ALPHANUM); got != "" {
		t.Errorf("RandomString(0): got %q, want empty", got)
	}
	if got := maniflex.RandomString(-5, maniflex.ALPHANUM); got != "" {
		t.Errorf("RandomString(-5): got %q, want empty", got)
	}
	if got := maniflex.RandomString(8, ""); got != "" {
		t.Errorf(`RandomString(8, ""): got %q, want empty`, got)
	}

	// Two independent 32-char draws must not collide (would signal a broken RNG).
	a := maniflex.RandomString(32, maniflex.ALPHANUM)
	b := maniflex.RandomString(32, maniflex.ALPHANUM)
	if a == b {
		t.Error("two 32-char random strings collided; RNG is not random")
	}
}
