// Package storage provides file storage backends for maniflex.
// LocalStorage is the built-in disk-based implementation.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"maniflex"
)

// LocalStorage stores files on the local filesystem under a root directory.
// File metadata is persisted as a sibling .meta.json file alongside each
// stored file, so Retrieve can return content type and filename without a
// separate metadata database.
//
// Key layout on disk:
//
//	{BasePath}/{key}            — the file itself
//	{BasePath}/{key}.meta.json  — JSON-encoded maniflex.FileMeta
type LocalStorage struct {
	basePath string // absolute path to the root directory
}

// NewLocalStorage creates a LocalStorage rooted at basePath.
// The directory is created if it does not exist.
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	abs, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve base path: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create base path: %w", err)
	}
	return &LocalStorage{basePath: abs}, nil
}

// Store writes the contents of r to the given key. The copy is cancelled when
// ctx is cancelled, leaving no partial file behind.
func (s *LocalStorage) Store(ctx context.Context, key string, r io.Reader, meta maniflex.FileMeta) error {
	if isMetaKey(key) {
		return fmt.Errorf("storage: key %q reserved for metadata sidecar", key)
	}
	fullPath, err := s.resolve(key, true)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("storage: create directories for %q: %w", key, err)
	}

	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("storage: create file %q: %w", key, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, ctxReader{ctx: ctx, r: r}); err != nil {
		os.Remove(fullPath)
		return fmt.Errorf("storage: write file %q: %w", key, err)
	}

	// Write metadata as sibling .meta.json
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: marshal metadata for %q: %w", key, err)
	}
	if err := os.WriteFile(fullPath+metaSuffix, metaBytes, 0o644); err != nil {
		os.Remove(fullPath)
		return fmt.Errorf("storage: write metadata for %q: %w", key, err)
	}

	return nil
}

// Retrieve returns a ReadCloser for the file at key, along with its metadata.
// Keys that end with `.meta.json` are rejected so the sidecar metadata file
// cannot be served as if it were stored content.
func (s *LocalStorage) Retrieve(_ context.Context, key string) (io.ReadCloser, maniflex.FileMeta, error) {
	if isMetaKey(key) {
		return nil, maniflex.FileMeta{}, maniflex.ErrFileNotFound
	}
	fullPath, err := s.resolve(key, false)
	if err != nil {
		return nil, maniflex.FileMeta{}, err
	}

	f, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, maniflex.FileMeta{}, maniflex.ErrFileNotFound
		}
		return nil, maniflex.FileMeta{}, fmt.Errorf("storage: open file %q: %w", key, err)
	}

	meta, err := s.readMeta(fullPath)
	if err != nil {
		f.Close()
		return nil, maniflex.FileMeta{}, err
	}

	return f, meta, nil
}

// Delete removes the file and its metadata from storage. Returns
// maniflex.ErrFileNotFound when the key does not exist so the standalone
// /files handler can translate that into a 404.
func (s *LocalStorage) Delete(_ context.Context, key string) error {
	fullPath, err := s.resolve(key, false)
	if err != nil {
		return err
	}

	primary := os.Remove(fullPath)
	if primary != nil && !os.IsNotExist(primary) {
		return fmt.Errorf("storage: delete file %q: %w", key, primary)
	}
	// Always try the metadata sidecar — it may exist even when the primary
	// file is missing (interrupted write) and we shouldn't leak it.
	if err := os.Remove(fullPath + metaSuffix); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete metadata for %q: %w", key, err)
	}
	if primary != nil && os.IsNotExist(primary) {
		return maniflex.ErrFileNotFound
	}
	return nil
}

// Exists reports whether the file at key exists in storage.
func (s *LocalStorage) Exists(_ context.Context, key string) (bool, error) {
	fullPath, err := s.resolve(key, false)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	return true, nil
}

// URL implements maniflex.FileStorage. LocalStorage does not support pre-signed
// URLs — it returns a server-relative /files/<key> path for both signed and
// public modes. The caller is responsible for serving that path (the framework
// mounts GET /files/* when FileStorage is configured). For time-limited access,
// integrate an HMAC-signed token at the application layer or switch to an
// S3-compatible backend.
func (s *LocalStorage) URL(_ context.Context, key string, _ time.Duration) (string, error) {
	if key == "" {
		return "", fmt.Errorf("storage: key must not be empty")
	}
	return "/files/" + key, nil
}

// Compile-time interface check.
var _ maniflex.FileStorage = (*LocalStorage)(nil)

// metaSuffix is the sibling-file extension for the JSON metadata sidecar
// written next to each stored file.
const metaSuffix = ".meta.json"

// isMetaKey reports whether key targets the internal metadata sidecar. We
// reject these keys from Store / Retrieve so clients cannot read or overwrite
// the framework's storage internals through the file handler.
func isMetaKey(key string) bool {
	return strings.HasSuffix(key, metaSuffix)
}

// ctxReader bridges a context.Context into an io.Reader so io.Copy stops
// the moment ctx is cancelled. Without this the Copy can block on a slow
// network upload past the request deadline / server shutdown.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// resolve maps a storage key to an absolute filesystem path, ensuring the
// result is contained within s.basePath (preventing path traversal attacks).
func (s *LocalStorage) resolve(key string, clean bool) (string, error) {
	if key == "" {
		return "", fmt.Errorf("storage: key must not be empty")
	}

	// Clean the key: normalise separators, collapse ".." sequences.
	cleaned := filepath.FromSlash("/" + key)
	if clean {
		cleaned = filepath.FromSlash(filepath.Clean("/" + key))
	}
	// filepath.Clean("/" + key) produces an absolute-looking path like "/a/b",
	// so strip the leading separator to make it relative to basePath.
	cleaned = strings.TrimPrefix(cleaned, string(filepath.Separator))

	full := filepath.Join(s.basePath, cleaned)

	// Verify the resolved path is still under basePath.
	if !strings.HasPrefix(full, s.basePath+string(filepath.Separator)) && full != s.basePath {
		return "", fmt.Errorf("storage: key %q resolves outside base path", key)
	}

	return full, nil
}

// readMeta reads the .meta.json sibling for the given file path.
func (s *LocalStorage) readMeta(filePath string) (maniflex.FileMeta, error) {
	metaPath := filePath + metaSuffix
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No metadata file — return empty metadata (best-effort).
			return maniflex.FileMeta{}, nil
		}
		return maniflex.FileMeta{}, fmt.Errorf("storage: read metadata: %w", err)
	}
	var meta maniflex.FileMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return maniflex.FileMeta{}, fmt.Errorf("storage: parse metadata: %w", err)
	}
	return meta, nil
}
