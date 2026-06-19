package sqlite

import (
	"errors"
	"testing"

	"github.com/xaleel/maniflex"
)

func TestNormalizeError_Kinds(t *testing.T) {
	cases := []struct {
		name     string
		msg      string
		wantKind maniflex.ConstraintKind
		wantCol  string
	}{
		{"unique", "UNIQUE constraint failed: users.email", maniflex.ConstraintUnique, "email"},
		{"not null", "NOT NULL constraint failed: notifications.emphasis", maniflex.ConstraintNotNull, "emphasis"},
		{"foreign key", "FOREIGN KEY constraint failed", maniflex.ConstraintForeignKey, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeError(errors.New(tc.msg), "users")
			var ce *maniflex.ErrConstraint
			if !errors.As(got, &ce) {
				t.Fatalf("want *maniflex.ErrConstraint, got %T (%v)", got, got)
			}
			if ce.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", ce.Kind, tc.wantKind)
			}
			if ce.Column != tc.wantCol {
				t.Errorf("Column = %q, want %q", ce.Column, tc.wantCol)
			}
		})
	}
}

// A non-constraint error must pass through unchanged.
func TestNormalizeError_PassThrough(t *testing.T) {
	in := errors.New("disk is full")
	if got := NormalizeError(in, "users"); got != in {
		t.Fatalf("non-constraint error should pass through, got %v", got)
	}
}
