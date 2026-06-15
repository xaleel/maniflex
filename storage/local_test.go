package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"maniflex"
)

func tempStorage(t *testing.T) *LocalStorage {
	t.Helper()
	dir := t.TempDir()
	s, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage(%q): %v", dir, err)
	}
	return s
}

func TestStoreRetrieveDelete(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	content := []byte("hello, world")
	meta := maniflex.FileMeta{
		Key:         "docs/test.txt",
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Filename:    "test.txt",
	}

	// Store
	if err := s.Store(ctx, "docs/test.txt", bytes.NewReader(content), meta); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Retrieve
	rc, gotMeta, err := s.Retrieve(ctx, "docs/test.txt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	rc.Close() // close before delete (Windows requires this)

	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	if gotMeta.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", gotMeta.ContentType, "text/plain")
	}
	if gotMeta.Filename != "test.txt" {
		t.Errorf("Filename = %q, want %q", gotMeta.Filename, "test.txt")
	}
	if gotMeta.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", gotMeta.Size, len(content))
	}

	// Delete
	if err := s.Delete(ctx, "docs/test.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify gone
	_, _, err = s.Retrieve(ctx, "docs/test.txt")
	if err != maniflex.ErrFileNotFound {
		t.Errorf("after delete, Retrieve err = %v, want ErrFileNotFound", err)
	}
}

func TestRetrieveNotFound(t *testing.T) {
	s := tempStorage(t)
	_, _, err := s.Retrieve(context.Background(), "nonexistent/file.txt")
	if err != maniflex.ErrFileNotFound {
		t.Errorf("Retrieve err = %v, want ErrFileNotFound", err)
	}
}

func TestDeleteMissingReportsNotFound(t *testing.T) {
	s := tempStorage(t)
	// Per the FileStorage contract (roadmap §11C.4), Delete returns
	// ErrFileNotFound for missing keys so the /files handler can surface a
	// 404 without an extra Exists round-trip. The TOCTOU race in the
	// previous Exists-then-Delete sequence — where two concurrent deletes
	// both passed Exists and one then saw a 500 — is gone.
	if err := s.Delete(context.Background(), "does/not/exist.txt"); err != maniflex.ErrFileNotFound {
		t.Errorf("Delete non-existent: got %v, want ErrFileNotFound", err)
	}
}

func TestNestedKeyPaths(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	key := "a/b/c/d/deeply/nested/file.bin"
	content := []byte{0x00, 0xFF, 0x42}
	meta := maniflex.FileMeta{Key: key, ContentType: "application/octet-stream", Size: 3, Filename: "file.bin"}

	if err := s.Store(ctx, key, bytes.NewReader(content), meta); err != nil {
		t.Fatalf("Store nested: %v", err)
	}

	rc, _, err := s.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve nested: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if !bytes.Equal(got, content) {
		t.Errorf("nested content mismatch")
	}
}

func TestPathTraversal(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	// Keys with ../ are normalised by resolve() so they stay within basePath.
	// Verify that "../../../escape.txt" is normalised to "escape.txt" (no escape).
	content := []byte("safe")
	err := s.Store(ctx, "../../../escape.txt", bytes.NewReader(content),
		maniflex.FileMeta{Key: "../../../escape.txt", Size: 4})
	if err != nil {
		t.Fatalf("Store normalised key: %v", err)
	}

	// The file should be retrievable at the normalised key "escape.txt"
	rc, _, err := s.Retrieve(ctx, "escape.txt")
	if err != nil {
		t.Fatalf("Retrieve normalised key: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "safe" {
		t.Errorf("content = %q, want %q", got, "safe")
	}

	// Verify the file was NOT written outside basePath
	outsidePath := filepath.Join(s.basePath, "..", "escape.txt")
	if _, err := os.Stat(outsidePath); err == nil {
		t.Error("file was written outside basePath — path traversal vulnerability!")
	}
}

func TestEmptyKey(t *testing.T) {
	s := tempStorage(t)
	err := s.Store(context.Background(), "", bytes.NewReader(nil), maniflex.FileMeta{})
	if err == nil {
		t.Error("expected error for empty key, got nil")
	}
}

func TestExists(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	// Should not exist yet
	exists, err := s.Exists(ctx, "check/me.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Error("Exists = true before Store")
	}

	// Store
	if err := s.Store(ctx, "check/me.txt", bytes.NewReader([]byte("hi")),
		maniflex.FileMeta{Key: "check/me.txt"}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Should exist now
	exists, err = s.Exists(ctx, "check/me.txt")
	if err != nil {
		t.Fatalf("Exists after Store: %v", err)
	}
	if !exists {
		t.Error("Exists = false after Store")
	}

	// Delete and check again
	s.Delete(ctx, "check/me.txt")
	exists, _ = s.Exists(ctx, "check/me.txt")
	if exists {
		t.Error("Exists = true after Delete")
	}
}

func TestConcurrentStore(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := filepath.Join("concurrent", string(rune('a'+n))+".txt")
			content := []byte("data")
			meta := maniflex.FileMeta{Key: key, Size: 4}
			if err := s.Store(ctx, key, bytes.NewReader(content), meta); err != nil {
				t.Errorf("concurrent Store %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestOverwriteExistingFile(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()
	key := "overwrite/file.txt"

	// Store original
	s.Store(ctx, key, bytes.NewReader([]byte("original")),
		maniflex.FileMeta{Key: key, ContentType: "text/plain", Size: 8, Filename: "file.txt"})

	// Overwrite
	s.Store(ctx, key, bytes.NewReader([]byte("updated")),
		maniflex.FileMeta{Key: key, ContentType: "text/plain", Size: 7, Filename: "file.txt"})

	rc, meta, err := s.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve after overwrite: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()

	if string(got) != "updated" {
		t.Errorf("content = %q, want %q", got, "updated")
	}
	if meta.Size != 7 {
		t.Errorf("meta.Size = %d, want 7", meta.Size)
	}
}

func TestURLReturnsFilesRoute(t *testing.T) {
	s := tempStorage(t)
	ctx := context.Background()

	// LocalStorage URL is independent of TTL — it always returns a
	// server-relative /files/<key> path that the file handler serves.
	got, err := s.URL(ctx, "uploads/abc/report.txt", time.Hour)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	want := "/files/uploads/abc/report.txt"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	// ttl=0 (FileACLPublic) also returns the same path — LocalStorage has
	// no real "signed" mode, so signed and public collapse to the same URL.
	got, err = s.URL(ctx, "uploads/abc/report.txt", 0)
	if err != nil {
		t.Fatalf("URL ttl=0: %v", err)
	}
	if got != want {
		t.Errorf("URL ttl=0 = %q, want %q", got, want)
	}
}

func TestURLRejectsEmptyKey(t *testing.T) {
	s := tempStorage(t)
	if _, err := s.URL(context.Background(), "", time.Hour); err == nil {
		t.Error("URL with empty key should error")
	}
}

func TestNewLocalStorageCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "nested", "dir")
	s, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}

	// Verify the directory was created
	info, err := os.Stat(s.basePath)
	if err != nil {
		t.Fatalf("Stat base path: %v", err)
	}
	if !info.IsDir() {
		t.Error("base path is not a directory")
	}
}
