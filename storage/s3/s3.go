// Package s3 provides an S3-compatible maniflex.FileStorage backend.
//
// It works against:
//
//   - AWS S3 — leave Config.Endpoint empty.
//   - MinIO — set Endpoint to the MinIO URL and UsePathStyle: true.
//   - Cloudflare R2 — set Endpoint to https://<account>.r2.cloudflarestorage.com
//     and leave UsePathStyle false.
//   - DigitalOcean Spaces, Wasabi, Backblaze B2 (S3 API), Ceph RGW — set the
//     vendor's endpoint URL; toggle UsePathStyle per their docs.
//
// Credentials follow the standard AWS resolution chain (env vars,
// shared config, IAM instance role, IRSA, ECS task role) unless an explicit
// AWSConfig is supplied.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"maniflex"
)

// Config configures an S3Storage. Bucket and Region are required.
type Config struct {
	// Bucket is the target S3 bucket name. Required.
	Bucket string

	// Region is the AWS region (e.g. "us-east-1"). Required for AWS S3; for
	// S3-compatible services (MinIO, R2) any non-empty value works.
	Region string

	// Endpoint is the service URL. Leave empty for AWS S3. Set to e.g.
	// "http://localhost:9000" for MinIO, or
	// "https://<account>.r2.cloudflarestorage.com" for Cloudflare R2.
	Endpoint string

	// UsePathStyle forces path-style addressing (bucket in the URL path).
	// Required for MinIO and most non-AWS S3 emulators. Default false uses
	// virtual-hosted style (bucket as subdomain) which is what AWS and R2 expect.
	UsePathStyle bool

	// KeyPrefix is prepended to every key transparently. Lets one bucket host
	// multiple environments (e.g. "staging/", "prod/") without colliding.
	// Trailing slash is normalised — both "envs/staging" and "envs/staging/"
	// produce the same keys. Empty (the default) means no prefix.
	KeyPrefix string

	// ACL is applied on Store. Common values: "private" (default), "public-read".
	// Leave empty to omit the header — useful when bucket policy handles ACLs.
	ACL string

	// AWSConfig overrides the auto-resolved aws.Config. Set this when you need
	// custom credentials providers, a non-standard HTTP client, or a custom
	// retry policy. Leave nil to use config.LoadDefaultConfig.
	AWSConfig *awsv2.Config
}

// S3Storage implements maniflex.FileStorage on top of an S3 API.
type S3Storage struct {
	cfg      Config
	client   s3API
	uploader uploaderAPI
	// presign generates a pre-signed GET URL for the given full (prefixed) S3
	// key and TTL. Nil when the backend was constructed without presigning
	// support (test seam via newWithClient without a presigner).
	presign func(ctx context.Context, fullKey string, ttl time.Duration) (string, error)
}

// s3API is the slice of the AWS SDK we depend on. Defining it as an interface
// lets tests inject a fake without spinning up a real HTTP server.
type s3API interface {
	PutObject(ctx context.Context, in *awss3.PutObjectInput, opts ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *awss3.GetObjectInput, opts ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *awss3.HeadObjectInput, opts ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput, opts ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
}

// uploaderAPI wraps the multipart-aware S3 uploader so Store streams large
// bodies in chunks instead of buffering the whole payload.
type uploaderAPI interface {
	Upload(ctx context.Context, in *awss3.PutObjectInput, opts ...func(*s3manager.Uploader)) (*s3manager.UploadOutput, error)
}

// New constructs an S3Storage. It validates the config, loads AWS credentials
// (unless cfg.AWSConfig is set), and builds the underlying S3 client.
//
//	store, err := s3.New(ctx, s3.Config{
//	    Bucket: "uploads",
//	    Region: "us-east-1",
//	})
//
// For MinIO:
//
//	store, err := s3.New(ctx, s3.Config{
//	    Bucket:       "uploads",
//	    Region:       "us-east-1",
//	    Endpoint:     "http://localhost:9000",
//	    UsePathStyle: true,
//	})
func New(ctx context.Context, cfg Config) (*S3Storage, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: Config.Bucket is required")
	}
	if cfg.Region == "" {
		return nil, errors.New("s3: Config.Region is required")
	}

	awsCfg := cfg.AWSConfig
	if awsCfg == nil {
		c, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
		if err != nil {
			return nil, fmt.Errorf("s3: load AWS config: %w", err)
		}
		awsCfg = &c
	}

	client := awss3.NewFromConfig(*awsCfg, func(o *awss3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = awsv2.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	uploader := s3manager.NewUploader(client)
	presignClient := awss3.NewPresignClient(client)

	s := newWithClient(cfg, client, uploader, nil)
	s.presign = func(ctx context.Context, fullKey string, ttl time.Duration) (string, error) {
		in := &awss3.GetObjectInput{
			Bucket: awsv2.String(cfg.Bucket),
			Key:    awsv2.String(fullKey),
		}
		var opts []func(*awss3.PresignOptions)
		if ttl > 0 {
			opts = append(opts, awss3.WithPresignExpires(ttl))
		}
		req, err := presignClient.PresignGetObject(ctx, in, opts...)
		if err != nil {
			return "", fmt.Errorf("s3: presign %q: %w", fullKey, err)
		}
		return req.URL, nil
	}
	return s, nil
}

// newWithClient is the test seam: lets callers inject a fake s3API and
// uploader without going through New (which requires real AWS credentials).
// presign may be nil — URL() will return an error in that case.
func newWithClient(cfg Config, client s3API, uploader uploaderAPI, presign func(context.Context, string, time.Duration) (string, error)) *S3Storage {
	cfg.KeyPrefix = normalisePrefix(cfg.KeyPrefix)
	return &S3Storage{cfg: cfg, client: client, uploader: uploader, presign: presign}
}

// Store implements maniflex.FileStorage. Uses the multipart uploader so the payload
// is streamed instead of buffered when it exceeds the part-size threshold.
func (s *S3Storage) Store(ctx context.Context, key string, r io.Reader, meta maniflex.FileMeta) error {
	full, err := s.fullKey(key)
	if err != nil {
		return err
	}

	in := &awss3.PutObjectInput{
		Bucket:   awsv2.String(s.cfg.Bucket),
		Key:      awsv2.String(full),
		Body:     r,
		Metadata: encodeMeta(meta),
	}
	if meta.ContentType != "" {
		in.ContentType = awsv2.String(meta.ContentType)
	}
	if s.cfg.ACL != "" {
		in.ACL = s3types.ObjectCannedACL(s.cfg.ACL)
	}

	if _, err := s.uploader.Upload(ctx, in); err != nil {
		return fmt.Errorf("s3: store %q: %w", key, err)
	}
	return nil
}

// Retrieve implements maniflex.FileStorage. Returns the underlying response body
// directly — callers stream from it.
func (s *S3Storage) Retrieve(ctx context.Context, key string) (io.ReadCloser, maniflex.FileMeta, error) {
	full, err := s.fullKey(key)
	if err != nil {
		return nil, maniflex.FileMeta{}, err
	}

	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: awsv2.String(s.cfg.Bucket),
		Key:    awsv2.String(full),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, maniflex.FileMeta{}, maniflex.ErrFileNotFound
		}
		return nil, maniflex.FileMeta{}, fmt.Errorf("s3: retrieve %q: %w", key, err)
	}

	meta := decodeMeta(out.Metadata)
	meta.Key = key // expose the caller's logical key, not the prefixed one
	if out.ContentType != nil {
		meta.ContentType = *out.ContentType
	}
	if out.ContentLength != nil {
		meta.Size = *out.ContentLength
	}
	if meta.Filename == "" {
		// Fall back to the key's basename when the original filename wasn't
		// persisted as object metadata.
		meta.Filename = basename(key)
	}
	return out.Body, meta, nil
}

// Delete implements maniflex.FileStorage. S3 returns success for missing keys, so
// the maniflex.FileStorage "idempotent" contract is honoured without extra work.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	full, err := s.fullKey(key)
	if err != nil {
		return err
	}
	if _, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: awsv2.String(s.cfg.Bucket),
		Key:    awsv2.String(full),
	}); err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// Exists implements maniflex.FileStorage. HeadObject is cheaper than GetObject —
// the body isn't transferred.
func (s *S3Storage) Exists(ctx context.Context, key string) (bool, error) {
	full, err := s.fullKey(key)
	if err != nil {
		return false, err
	}
	if _, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: awsv2.String(s.cfg.Bucket),
		Key:    awsv2.String(full),
	}); err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("s3: exists %q: %w", key, err)
	}
	return true, nil
}

// URL implements maniflex.FileStorage. Returns a presigned GET URL for the given key
// valid for ttl. When ttl is 0 (FileACLPublic), a long-lived presigned URL is
// returned (AWS limits presigned URLs to 7 days; use bucket/object public-read
// ACL for truly permanent URLs).
// Returns an error if the backend was constructed via newWithClient without a
// presigner (test-only path).
func (s *S3Storage) URL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if s.presign == nil {
		return "", fmt.Errorf("s3: presigning not configured")
	}
	full, err := s.fullKey(key)
	if err != nil {
		return "", err
	}
	if ttl == 0 {
		ttl = 7 * 24 * time.Hour // AWS max presign duration
	}
	return s.presign(ctx, full, ttl)
}

// fullKey applies KeyPrefix and rejects keys that would escape the bucket
// namespace. Matches LocalStorage semantics: empty / leading-slash keys are
// rejected so behaviour is consistent across backends.
func (s *S3Storage) fullKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("s3: key must not be empty")
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("s3: key %q must not begin with '/'", key)
	}
	if s.cfg.KeyPrefix == "" {
		return key, nil
	}
	return s.cfg.KeyPrefix + key, nil
}

// normalisePrefix ensures a non-empty prefix ends with exactly one slash.
func normalisePrefix(p string) string {
	if p == "" {
		return ""
	}
	return strings.TrimRight(p, "/") + "/"
}

func basename(key string) string {
	if i := strings.LastIndexByte(key, '/'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// encodeMeta packs FileMeta into S3 user metadata. AWS lowercases user
// metadata keys; choose short stable names rather than relying on case.
func encodeMeta(m maniflex.FileMeta) map[string]string {
	out := make(map[string]string, 3)
	if m.Filename != "" {
		out["mfx-filename"] = m.Filename
	}
	if m.Size > 0 {
		out["mfx-size"] = strconv.FormatInt(m.Size, 10)
	}
	if m.ContentType != "" {
		// Stored separately as Content-Type, but also persist in user metadata
		// in case the bucket's CORS/CDN strips Content-Type on serve.
		out["mfx-content-type"] = m.ContentType
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func decodeMeta(headers map[string]string) maniflex.FileMeta {
	var m maniflex.FileMeta
	if headers == nil {
		return m
	}
	if v, ok := lookupCI(headers, "mfx-filename"); ok {
		m.Filename = v
	}
	if v, ok := lookupCI(headers, "mfx-size"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			m.Size = n
		}
	}
	if v, ok := lookupCI(headers, "mfx-content-type"); ok {
		m.ContentType = v
	}
	return m
}

// lookupCI does a case-insensitive map lookup. S3 user metadata is returned
// with various casings depending on the implementation (lowercase from AWS,
// mixed-case from MinIO); this normalises across them.
func lookupCI(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	lower := strings.ToLower(key)
	for k, v := range m {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return "", false
}

// isNoSuchKey returns true for the SDK errors S3 emits when a key is missing.
// Covers both the typed *types.NoSuchKey (GetObject) and the bare 404
// SmithyHTTPError (HeadObject — S3 returns 404 without a typed body).
func isNoSuchKey(err error) bool {
	if err == nil {
		return false
	}
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *s3types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	// HeadObject — smithy returns a generic API error with code "NotFound".
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// Compile-time interface check.
var _ maniflex.FileStorage = (*S3Storage)(nil)

// silence unused import when tests don't use bytes — left in for the
// test-only newWithClient helper signature.
var _ = bytes.NewReader
