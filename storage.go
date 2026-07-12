package maniflex

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
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

	// DefaultMaxUploadBytes caps the total size of a multipart/form-data request
	// when FilesConfig.MaxUploadBytes is zero. Without a ceiling the body is
	// unbounded: everything past the in-memory buffer spools to temp files, so a
	// single long stream can fill the disk (BUG-5).
	DefaultMaxUploadBytes int64 = 32 << 20 // 32 MB

	// DefaultMaxUploadMemory is how much of a multipart request is buffered in
	// memory before the remainder spools to temp files, when
	// FilesConfig.MaxUploadMemory is zero. It matches net/http's own default.
	DefaultMaxUploadMemory int64 = 32 << 20 // 32 MB
)

// uploadLimitOr / uploadMemoryOr resolve a configured multipart limit, falling
// back to the framework default when it is left at zero.
func uploadLimitOr(configured int64) int64 {
	if configured > 0 {
		return configured
	}
	return DefaultMaxUploadBytes
}

func uploadMemoryOr(configured int64) int64 {
	if configured > 0 {
		return configured
	}
	return DefaultMaxUploadMemory
}

func (c FilesConfig) uploadLimit() int64  { return uploadLimitOr(c.MaxUploadBytes) }
func (c FilesConfig) uploadMemory() int64 { return uploadMemoryOr(c.MaxUploadMemory) }

// FileMeta describes an uploaded file's metadata.
type FileMeta struct {
	Key         string `json:"key"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	Filename    string `json:"filename"`
}

// FilesConfig groups all file-upload/storage settings under Config.FilesConfig.
// It supersedes the flat Config.FileStorage / FileSignedURLTTL / FileMiddleware
// fields removed in this release.
//
// Minimal setup — enable model file fields and the standalone /files endpoints:
//
//	maniflex.New(maniflex.Config{
//	    FilesConfig: maniflex.FilesConfig{
//	        Storage:        storage.NewLocalStorage("./uploads"),
//	        MountEndpoints: true, // required: see the field doc below
//	    },
//	})
type FilesConfig struct {
	// Storage is the storage backend for file uploads. When nil, models with
	// mfx:"file" fields reject multipart uploads with 501, and the standalone
	// /files endpoints (if mounted) return 501. Set before calling Start(), or
	// use SetStorage() for two-step init.
	//
	// Use storage.NewLocalStorage(path) for disk-based storage, or implement
	// the FileStorage interface for S3, R2, GCS, etc.
	Storage FileStorage

	// MountEndpoints controls whether the standalone /files routes (POST /files,
	// GET /files/*, DELETE /files/*) are mounted. It defaults to false and is
	// NOT implied by setting Storage — you must opt in explicitly.
	//
	// Footgun: setting only Storage enables per-model attachment routes and
	// multipart handling on mfx:"file" fields, but leaves the standalone /files
	// endpoints unmounted (requests 404). If you relied on the pre-FilesConfig
	// behaviour where a non-nil FileStorage auto-mounted /files, set this true.
	//
	// Mounting with Storage still nil is allowed: the routes exist but every
	// request returns 501 NO_STORAGE until a backend is configured.
	MountEndpoints bool

	// SignedURLTTL is the default time-to-live for pre-signed URLs generated
	// for mfx:"file_acl:signed" fields. Default: DefaultFileSignedURLTTL (1 hour).
	SignedURLTTL time.Duration

	// MaxUploadBytes caps the total size of a multipart/form-data request — every
	// part summed, not per file. A request over the ceiling is rejected with
	// 413 BODY_TOO_LARGE as it streams, before anything is written to disk.
	// Default: DefaultMaxUploadBytes (32 MB). It applies to model create/update
	// uploads and to POST /files alike.
	//
	// The per-field mfx:"max_size" tag still applies on top of this: it bounds an
	// individual attachment, this bounds the request that carries it. Raise this
	// for large media, and note that a Deserialize-step body.MaxBodySize
	// (ctx.SetMaxBodySize) overrides it for the models it is registered on.
	MaxUploadBytes int64

	// MaxUploadMemory is how much of a multipart request is buffered in memory
	// before the remainder spools to temp files. Default: DefaultMaxUploadMemory
	// (32 MB). Lower it to trade memory for temp-file I/O; the total is still
	// bounded by MaxUploadBytes either way.
	MaxUploadMemory int64

	// KeyGen derives the storage key for a standalone POST /files upload from the
	// request context and the multipart header. When nil, DefaultKeyGen is used,
	// producing "uploads/<uuid>/<sanitised-filename>". Override it to route
	// uploads into a custom layout (e.g. per-tenant prefixes read from ctx.Auth).
	//
	// The returned string is used verbatim as the storage key; implementations
	// are responsible for sanitising any user-supplied component (see
	// sanitizeFilename / DefaultKeyGen for the framework's default).
	KeyGen func(ctx *ServerContext, header *multipart.FileHeader) string

	// BeforeMiddlewares run before the standalone /files endpoints (POST /files,
	// GET /files/*, DELETE /files/*) with the supplied pipeline middleware
	// chain. Middlewares run in slice order; any that sets ctx.Response
	// short-circuits the request before the file handler runs.
	//
	// Empty (the default) leaves /files unauthenticated — backward-compatible
	// with pre-fix behaviour but unsafe for production: anyone who guesses a
	// key can delete arbitrary files. Recommended production setup:
	//
	//	maniflex.Config{
	//	    FilesConfig: maniflex.FilesConfig{
	//	        Storage:        storage.NewLocalStorage("./uploads"),
	//	        MountEndpoints: true,
	//	        BeforeMiddlewares: []maniflex.MiddlewareFunc{
	//	            auth.JWTAuth("...", auth.JWTOptions{}),
	//	            auth.RequireRole("admin"),
	//	        },
	//	    },
	//	}
	//
	// Per-model attachment routes (mfx:"file" fields on a registered model)
	// already run through the full Auth / DB pipeline and are unaffected.
	BeforeMiddlewares []MiddlewareFunc

	// AfterMiddlewares run after the standalone /files handler has served the
	// request. They are for observation and side effects only — audit logging,
	// metrics, cleanup — NOT for altering the response.
	//
	// The handler streams its response (status, headers, body) directly to the
	// client, so by the time an AfterMiddleware runs the response is already
	// committed to the wire and cannot be rewritten. Read the outcome via
	// ctx.Writer.(interface{ Status() int }).Status(); a middleware that sets
	// ctx.Response here is ignored (and logged) rather than corrupting the
	// already-sent body. To alter or replace the response, or to block the
	// request, use BeforeMiddlewares instead.
	//
	// Like BeforeMiddlewares, each runs in slice order and receives a next()
	// callback it must invoke to run the remaining chain.
	AfterMiddlewares []MiddlewareFunc
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
