package maniflex

// Keyset pagination walks (cursor field, id) with a > / < boundary predicate.
// That comparison is never true for NULL, and the drivers disagree about where
// NULLs even sort (Postgres last on ASC, SQLite first), so a nullable cursor
// column silently skips or repeats rows. Registration must refuse it (BUG-8).

import (
	"strings"
	"testing"
	"time"
)

type cursorOKDoc struct {
	BaseModel
	PublishedAt time.Time `json:"published_at" db:"published_at" mfx:"sortable,cursor_field:published_at"`
}

type cursorNullableDoc struct {
	BaseModel
	PublishedAt *time.Time `json:"published_at" db:"published_at" mfx:"sortable,cursor_field:published_at"`
}

type cursorNotSortableDoc struct {
	BaseModel
	Seq int `json:"seq" db:"seq" mfx:"cursor_field:seq"`
}

func TestCollectCursorField_NullableFieldRejected(t *testing.T) {
	srv := New(Config{})
	err := srv.Register(cursorNullableDoc{})
	if err == nil {
		t.Fatal("registering a model with a nullable cursor_field must fail")
	}
	if !strings.Contains(err.Error(), "nullable") {
		t.Errorf("error should explain the column is nullable, got: %v", err)
	}
}

func TestCollectCursorField_NonNullFieldAccepted(t *testing.T) {
	srv := New(Config{})
	if err := srv.Register(cursorOKDoc{}); err != nil {
		t.Fatalf("a NOT NULL sortable cursor_field must register: %v", err)
	}
	meta, ok := srv.Registry().Get("cursorOKDoc")
	if !ok {
		t.Fatal("model not registered")
	}
	if meta.CursorField != "published_at" {
		t.Errorf("CursorField = %q, want published_at", meta.CursorField)
	}
}

// The pre-existing sortable requirement still applies — the nullability check
// must not have displaced it.
func TestCollectCursorField_NonSortableFieldRejected(t *testing.T) {
	srv := New(Config{})
	err := srv.Register(cursorNotSortableDoc{})
	if err == nil {
		t.Fatal("registering a non-sortable cursor_field must fail")
	}
	if !strings.Contains(err.Error(), "sortable") {
		t.Errorf("error should mention sortable, got: %v", err)
	}
}
