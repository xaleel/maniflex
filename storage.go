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

	// KeyScope binds a minted storage key to the principal that minted it, so a
	// later create/update that references the key by name can be refused when the
	// referencing principal is not the one it was minted for. Without this, any
	// caller who learns a key (they leak through signed URLs and file_acl:private
	// responses) can point their own record at another record's — or another
	// tenant's — object, and an auto_delete field can then delete a blob it does
	// not own.
	//
	// KeyScope returns an opaque token identifying the owner of keys minted in
	// this request. Every minting path (POST /files, a presigned upload, and a
	// multipart upload through a model) prefixes the key with a hash of the token;
	// handleFileKeyReference and the FileKeys list verifier recompute the token
	// from the referencing request and refuse a key whose prefix does not match.
	//
	// When nil, the framework uses ctx.Auth.TenantID when set (so a tenant's
	// members share one scope), else ctx.Auth.UserID. An empty token — the return
	// for an anonymous request, and what you return to opt a request out — leaves
	// the key unscoped and referenceable by anyone, so the guarantee holds only
	// for keys minted while a principal was present. A key minted before this
	// release carries no scope prefix and is likewise left to the existence check,
	// so upgrading does not break references to already-stored keys.
	//
	// The token must resolve consistently across the mint request and the later
	// reference request; if your /files auth and your model auth populate ctx.Auth
	// differently, override KeyScope to read whatever both share.
	KeyScope func(ctx *ServerContext) string

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
	//
	// meta.Size is the object's length in bytes when known, and 0 when it is not:
	// a streamed upload (mfx:"file,upload:stream" or POST /files) is piped through
	// before its length is known, so the backend must store an unsized reader —
	// read r to EOF rather than trusting meta.Size. On S3 this means a multipart
	// upload; the AWS SDK's manager.Uploader does it transparently.
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

	// Stat returns the metadata of the object at key without fetching its body.
	// Returns ErrFileNotFound when the key does not exist.
	//
	// Size and ContentType must reflect what is actually stored, because they are
	// what the framework checks an mfx:"file" field's max_size and accept rules
	// against when a record references a key uploaded out of band — a client that
	// uploaded 5 GB to a 60 MB field is caught here or nowhere.
	//
	// It exists because Exists answers only yes/no and Retrieve would download the
	// object to read its size, which for the large files this path is for is the
	// cost the path was created to avoid.
	Stat(ctx context.Context, key string) (FileMeta, error)

	// PresignUpload returns a one-shot authorisation for a client to write one
	// object directly to storage at key, bypassing the app process entirely.
	//
	// Return ErrPresignUnsupported when the backend cannot mint one. Do NOT return
	// an unauthenticated URL instead: a presigned upload that degrades to an open
	// one is a hole, not a fallback, and the caller answers 501 rather than
	// handing a client something that only looks signed.
	//
	// A backend that can pin opts.MaxSize into the signature must do so (S3's
	// POST-policy content-length-range does; a presigned PUT cannot). The
	// framework re-checks the stored object on completion regardless, so a backend
	// that cannot pin it is not unsafe — but the bytes have to be paid for and
	// stored before it can be caught, so pin it where you can.
	PresignUpload(ctx context.Context, key string, opts PresignUploadOptions) (*PresignedUpload, error)

	// URL returns a URL suitable for the given access mode.
	// For FileACLSigned, ttl is how long the URL remains valid; the backend
	// must return an error if it cannot produce time-limited URLs.
	// For FileACLPublic, ttl is 0 and a permanent (or very long-lived) URL
	// is returned.
	// LocalStorage returns a server-relative /files/<key> path for both modes.
	// S3Storage returns a presigned GET URL using the AWS SDK presigner.
	URL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// RangeRetriever is an optional extension a FileStorage backend may implement
// to serve a byte window of an object without transferring the whole thing.
//
// When a backend implements it, GET /files/* and the per-model attachment route
// answer a Range request with 206 Partial Content, fetching only the requested
// bytes from storage — on a remote backend that is the difference between
// paying egress for a 2 GB video and paying it for the 1 MB the client seeked
// to. A backend that does not implement it still works: the framework falls
// back to net/http's ServeContent when the reader is seekable, and otherwise
// ignores the Range header and serves the whole object with 200.
//
// It is deliberately separate from FileStorage so that adding it is not a
// breaking change for third-party backends. LocalStorage and S3Storage both
// implement it.
type RangeRetriever interface {
	// RetrieveRange returns a reader over exactly the bytes
	// [offset, offset+length) of the object at key, along with its metadata.
	//
	// The framework resolves the window against the size reported by Stat
	// before calling, so offset and length are always absolute, in range, and
	// length is always positive — implementations do not need to clamp or to
	// interpret the relative forms of a Range header. The returned FileMeta may
	// describe the window rather than the object (its Size in particular); the
	// framework takes the object's content type and filename from Stat when the
	// window does not carry them.
	//
	// Returns ErrFileNotFound when the key does not exist.
	RetrieveRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, FileMeta, error)
}

// ErrFileNotFound is returned by FileStorage.Retrieve when the requested key
// does not exist.
var ErrFileNotFound = errors.New("file not found")

// ErrPresignUnsupported is returned by FileStorage.PresignUpload from a backend
// that cannot mint a presigned upload. The upload-url route answers 501 with it.
//
// Returning this is the correct, safe answer — the wrong one is an unsigned URL,
// which would be an unauthenticated write endpoint wearing a presigned URL's
// clothes. LocalStorage.URL already sets that precedent on the read side (it
// returns /files/<key> whatever the ttl), and a read served openly is a leak
// where a write served openly is an upload endpoint for the internet.
var ErrPresignUnsupported = errors.New("storage backend cannot presign uploads")

// PresignUploadOptions describes the upload a presigned request must permit. The
// framework fills it from the target mfx:"file" field's tags.
type PresignUploadOptions struct {
	// TTL is how long the presigned request stays valid.
	TTL time.Duration

	// MaxSize caps the object in bytes, from mfx:"max_size". Zero means no cap.
	// Pin it into the signature if the backend can (S3 POST-policy's
	// content-length-range); the framework re-checks on completion either way.
	MaxSize int64

	// ContentType is the media type the client declared at mint time. It has
	// already been checked against the field's mfx:"accept" list, so a backend
	// that can bind the signature to it should: that is what stops a client
	// declaring video/mp4 to get the URL and then uploading something else.
	ContentType string

	// Filename is the client's original filename, for a Content-Disposition the
	// backend may wish to store. Already sanitised into the key.
	Filename string
}

// PresignedUpload is a one-shot authorisation for a client to write one object
// directly to storage. It is what the upload-url route returns.
type PresignedUpload struct {
	// URL is where the client sends the upload.
	URL string `json:"url"`

	// Method is the HTTP method to use — "POST" for an S3 POST-policy form,
	// "PUT" for a presigned PUT.
	Method string `json:"method"`

	// Fields are form values the client must send alongside the file, in this
	// order, as multipart/form-data with the file last. POST-policy only.
	Fields map[string]string `json:"fields,omitempty"`

	// Headers are headers the client must set verbatim. Presigned PUT only.
	Headers map[string]string `json:"headers,omitempty"`

	// Key is the storage key the object will land at — the value to send back in
	// the record's file field to complete the upload. The framework mints it; a
	// client never chooses it, or it could aim an upload at another record's
	// object.
	Key string `json:"key"`

	// ExpiresAt is when the authorisation stops working.
	ExpiresAt time.Time `json:"expires_at"`

	// MaxSize echoes the cap the signature pins, so a client can fail a too-large
	// file before spending the upload rather than after.
	MaxSize int64 `json:"max_size,omitempty"`
}

// UploadedFile holds a parsed file from a multipart request, ready for
// processing by the Service step.
type UploadedFile struct {
	Filename    string
	ContentType string
	Size        int64
	Reader      io.ReadCloser
}
