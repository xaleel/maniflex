package testutil

import (
	"fmt"
	"testing"
)

// AssertEqual fails the test if got != want.
func AssertEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

// AssertNotEmpty fails the test if s is empty.
func AssertNotEmpty(t *testing.T, label, s string) {
	t.Helper()
	if s == "" {
		t.Errorf("%s: expected non-empty string", label)
	}
}

// AssertContains fails the test if the slice does not contain want.
func AssertContains[T comparable](t *testing.T, label string, slice []T, want T) {
	t.Helper()
	for _, v := range slice {
		if v == want {
			return
		}
	}
	t.Errorf("%s: %v not found in %v", label, want, slice)
}

// AssertLen fails the test if len(slice) != want.
func AssertLen(t *testing.T, label string, got []any, want int) {
	t.Helper()
	if len(got) != want {
		t.Errorf("%s: length got %d, want %d", label, len(got), want)
	}
}

// Field is a helper to extract a string field from a map[string]any.
func Field(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("field %q not present in %v", key, m)
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

// FloatField extracts a numeric field as float64.
func FloatField(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("field %q not present in %v", key, m)
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case nil:
		return float64(0)
	}
	t.Errorf("field %q: cannot convert %T to float64", key, v)
	return 0
}

// BoolField extracts a boolean field.
func BoolField(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Errorf("field %q not present in %v", key, m)
		return false
	}
	b, ok := v.(bool)
	if !ok {
		t.Errorf("field %q: cannot convert %T to bool", key, v)
		return false
	}
	return b
}

// Ptr returns a pointer to t (for use with *bool config fields).
func Ptr[T any](v T) *T { return &v }
