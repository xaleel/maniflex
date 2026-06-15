package maniflex

import (
	"context"
	"errors"
	"io"
	"time"
)

// FileACLMode controls how file field values are presented in API responses.
type FileACLMode string

const (
	// FileACLPrivate is the default: the storage key is returned as-is.
	// Downloads go through the per-model attachment route (3B.3a) which
	// enforces the same auth as the parent record read.
	FileACLPrivate FileACLMode = "private"

	// FileACLSigned causes the Response step to replace the storage key with
	// a pre-signed URL valid for Config.FileSignedURLTTL (default 1 hour).
	// Requires FileStorage.URL() support on the configured backend.
	FileACLSigned FileACLMode = "signed"

	// FileACLPublic causes the Response step to replace the storage key with
	// a permanent public URL. LocalStorage returns a /files/<key> path;
	// S3Storage returns the object's public URL (requires public-read ACL on
	// the bucket or object).
	FileACLPublic FileACLMode = "public"

	// DefaultFileSignedURLTTL is the default TTL for signed URLs when
	// Config.FileSignedURLTTL is zero.
	DefaultFileSignedURLTTL = time.Hour
)

// FileMeta describes an uploaded file's metadata.
type FileMeta struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Filename    string `json:"filename"`
}

// FileStorage is the interface all file storage backends must implement.
// It is analogous to DBAdapter for database operations.
//
// Implementations:
//   - storage.LocalStorage — disk-based, ships with maniflex
//   - Bring your own: S3, R2, GCS, etc.
type FileStorage interface {
	// Store writes the contents of r to the given key with the supplied metadata.
	// The key is framework-generated; implementations must create any intermediate
	// directories or object prefixes as needed.
	Store(ctx context.Context, key string, r io.Reader, meta FileMeta) error

	// Retrieve returns a ReadCloser for the file at key, along with its metadata.
	// Returns ErrFileNotFound when the key does not exist.
	Retrieve(ctx context.Context, key string) (io.ReadCloser, FileMeta, error)

	// Delete removes the file at key from storage. Implementations should
	// return ErrFileNotFound when the key does not exist so the standalone
	// DELETE /files/* handler can translate that into a 404. Returning nil
	// for a missing key is also acceptable for backends that cannot detect
	// it atomically (notably some S3-compatible APIs); callers must treat
	// both as "delete succeeded" except where a 404 is specifically wanted.
	Delete(ctx context.Context, key string) error

	// Exists reports whether the file at key exists in storage.
	Exists(ctx context.Context, key string) (bool, error)

	// URL returns a URL suitable for the given access mode.
	// For FileACLSigned, ttl is how long the URL remains valid; the backend
	// must return an error if it cannot produce time-limited URLs.
	// For FileACLPublic, ttl is 0 and a permanent (or very long-lived) URL
	// is returned.
	// LocalStorage returns a server-relative /files/<key> path for both modes.
	// S3Storage returns a presigned GET URL using the AWS SDK presigner.
	URL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// ErrFileNotFound is returned by FileStorage.Retrieve when the requested key
// does not exist.
var ErrFileNotFound = errors.New("file not found")

// UploadedFile holds a parsed file from a multipart request, ready for
// processing by the Service step.
type UploadedFile struct {
	Filename    string
	ContentType string
	Size        int64
	Reader      io.ReadCloser
}
