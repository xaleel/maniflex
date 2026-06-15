package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/xaleel/maniflex"
)

// fakeS3 is an in-memory s3API + uploaderAPI used by the unit tests.
type fakeS3 struct {
	objects map[string]fakeObject
	// puts captures every Upload/PutObject call so tests can inspect inputs.
	puts []*awss3.PutObjectInput
}

type fakeObject struct {
	body        []byte
	contentType string
	metadata    map[string]string
	acl         string
}

func newFake() *fakeS3 { return &fakeS3{objects: map[string]fakeObject{}} }

func (f *fakeS3) Upload(_ context.Context, in *awss3.PutObjectInput, _ ...func(*s3manager.Uploader)) (*s3manager.UploadOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	o := fakeObject{body: body, metadata: in.Metadata}
	if in.ContentType != nil {
		o.contentType = *in.ContentType
	}
	o.acl = string(in.ACL)
	f.objects[*in.Key] = o
	cp := *in
	f.puts = append(f.puts, &cp)
	return &s3manager.UploadOutput{}, nil
}

func (f *fakeS3) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	_, err := f.Upload(ctx, in)
	return &awss3.PutObjectOutput{}, err
}

func (f *fakeS3) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	o, ok := f.objects[*in.Key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	size := int64(len(o.body))
	return &awss3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(o.body)),
		ContentType:   strPtrOrNil(o.contentType),
		ContentLength: &size,
		Metadata:      o.metadata,
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if _, ok := f.objects[*in.Key]; !ok {
		return nil, &s3types.NotFound{}
	}
	return &awss3.HeadObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	delete(f.objects, *in.Key)
	return &awss3.DeleteObjectOutput{}, nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func newStore(cfg Config) (*S3Storage, *fakeS3) {
	if cfg.Bucket == "" {
		cfg.Bucket = "test"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	f := newFake()
	return newWithClient(cfg, f, f, nil), f
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestNew_validates_required_fields(t *testing.T) {
	if _, err := New(context.Background(), Config{Region: "us-east-1"}); err == nil {
		t.Error("expected error for empty bucket, got nil")
	}
	if _, err := New(context.Background(), Config{Bucket: "b"}); err == nil {
		t.Error("expected error for empty region, got nil")
	}
}

func TestStoreRetrieveDelete_RoundTrip(t *testing.T) {
	store, fake := newStore(Config{})
	ctx := context.Background()

	content := []byte("hello world")
	meta := maniflex.FileMeta{
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Filename:    "hello.txt",
	}

	if err := store.Store(ctx, "docs/hello.txt", bytes.NewReader(content), meta); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Inspect what was sent to S3.
	if len(fake.puts) != 1 {
		t.Fatalf("expected 1 PutObject call, got %d", len(fake.puts))
	}
	put := fake.puts[0]
	if *put.Key != "docs/hello.txt" {
		t.Errorf("Key sent to S3: got %q want %q", *put.Key, "docs/hello.txt")
	}
	if put.ContentType == nil || *put.ContentType != "text/plain" {
		t.Errorf("ContentType: got %v want text/plain", put.ContentType)
	}
	if put.Metadata["mfx-filename"] != "hello.txt" {
		t.Errorf("mfx-filename metadata: got %q want hello.txt", put.Metadata["mfx-filename"])
	}

	// Retrieve and verify content + reconstructed meta.
	rc, gotMeta, err := store.Retrieve(ctx, "docs/hello.txt")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("body: got %q want %q", got, content)
	}
	if gotMeta.Key != "docs/hello.txt" {
		t.Errorf("returned Key must be the caller's logical key; got %q", gotMeta.Key)
	}
	if gotMeta.ContentType != "text/plain" {
		t.Errorf("ContentType: got %q want text/plain", gotMeta.ContentType)
	}
	if gotMeta.Size != int64(len(content)) {
		t.Errorf("Size: got %d want %d", gotMeta.Size, len(content))
	}
	if gotMeta.Filename != "hello.txt" {
		t.Errorf("Filename: got %q want hello.txt", gotMeta.Filename)
	}

	// Delete and verify gone.
	if err := store.Delete(ctx, "docs/hello.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := store.Retrieve(ctx, "docs/hello.txt"); !errors.Is(err, maniflex.ErrFileNotFound) {
		t.Errorf("after delete, Retrieve err = %v, want ErrFileNotFound", err)
	}
}

func TestRetrieve_NotFound_ReturnsErrFileNotFound(t *testing.T) {
	store, _ := newStore(Config{})
	_, _, err := store.Retrieve(context.Background(), "missing/key.bin")
	if !errors.Is(err, maniflex.ErrFileNotFound) {
		t.Errorf("err = %v, want maniflex.ErrFileNotFound", err)
	}
}

func TestDelete_IsIdempotent(t *testing.T) {
	store, _ := newStore(Config{})
	// S3's DeleteObject always succeeds; the adapter mirrors that.
	if err := store.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("delete on missing key: %v", err)
	}
}

func TestExists_TrueAfterStore_FalseAfterDelete(t *testing.T) {
	store, _ := newStore(Config{})
	ctx := context.Background()
	key := "ex/test"

	if exists, _ := store.Exists(ctx, key); exists {
		t.Error("exists before store")
	}
	store.Store(ctx, key, bytes.NewReader([]byte("x")), maniflex.FileMeta{Size: 1})
	if exists, err := store.Exists(ctx, key); err != nil || !exists {
		t.Errorf("after Store: exists=%v err=%v", exists, err)
	}
	store.Delete(ctx, key)
	if exists, _ := store.Exists(ctx, key); exists {
		t.Error("exists after delete")
	}
}

func TestKeyPrefix_AppliedTransparently(t *testing.T) {
	store, fake := newStore(Config{KeyPrefix: "envs/staging"}) // missing trailing /
	ctx := context.Background()

	if err := store.Store(ctx, "logo.png", bytes.NewReader([]byte("png")),
		maniflex.FileMeta{ContentType: "image/png", Size: 3}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// The fake sees the prefixed key.
	if _, ok := fake.objects["envs/staging/logo.png"]; !ok {
		t.Errorf("prefixed object not stored; keys = %v", keysOf(fake.objects))
	}

	// The caller sees their original key on Retrieve.
	_, meta, err := store.Retrieve(ctx, "logo.png")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if meta.Key != "logo.png" {
		t.Errorf("returned Key: got %q want logo.png (prefix must be stripped)", meta.Key)
	}
}

func TestStore_AppliesACL(t *testing.T) {
	store, fake := newStore(Config{ACL: "public-read"})
	if err := store.Store(context.Background(), "k", bytes.NewReader([]byte("d")),
		maniflex.FileMeta{Size: 1}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if fake.objects["k"].acl != "public-read" {
		t.Errorf("ACL: got %q want public-read", fake.objects["k"].acl)
	}
}

func TestFullKey_RejectsEmptyAndLeadingSlash(t *testing.T) {
	store, _ := newStore(Config{})
	ctx := context.Background()

	if err := store.Store(ctx, "", nil, maniflex.FileMeta{}); err == nil {
		t.Error("empty key must be rejected")
	}
	if err := store.Store(ctx, "/leading", bytes.NewReader([]byte("x")),
		maniflex.FileMeta{Size: 1}); err == nil {
		t.Error("leading-slash key must be rejected")
	}
}

func TestRetrieve_FilenameFallsBackToBasename(t *testing.T) {
	store, fake := newStore(Config{})
	// Store with no Filename metadata — Retrieve should derive it from the key.
	fake.objects["a/b/c.bin"] = fakeObject{body: []byte("x")}
	_, meta, err := store.Retrieve(context.Background(), "a/b/c.bin")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if meta.Filename != "c.bin" {
		t.Errorf("Filename fallback: got %q want c.bin", meta.Filename)
	}
}

func TestMetadata_CaseInsensitiveLookup(t *testing.T) {
	// MinIO returns user metadata with leading capital letters; AWS lowercases.
	// Either should decode correctly.
	store, fake := newStore(Config{})
	fake.objects["k"] = fakeObject{
		body: []byte("x"),
		metadata: map[string]string{
			"Mfx-Filename": "actual.bin",
		},
	}
	_, meta, _ := store.Retrieve(context.Background(), "k")
	if meta.Filename != "actual.bin" {
		t.Errorf("case-insensitive metadata: got %q want actual.bin", meta.Filename)
	}
}

func TestNormalisePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"":          "",
		"a":         "a/",
		"a/":        "a/",
		"a/b":       "a/b/",
		"a/b///":    "a/b/",
	} {
		if got := normalisePrefix(in); got != want {
			t.Errorf("normalisePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// newStoreWithPresign returns a store wired with a fake presigner that records
// the calls and returns a deterministic URL — enough to test the URL() ttl
// handling without invoking the AWS SDK.
func newStoreWithPresign(cfg Config) (*S3Storage, *fakeS3, *[]presignCall) {
	if cfg.Bucket == "" {
		cfg.Bucket = "test"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	f := newFake()
	calls := &[]presignCall{}
	presign := func(_ context.Context, fullKey string, ttl time.Duration) (string, error) {
		*calls = append(*calls, presignCall{key: fullKey, ttl: ttl})
		return fmt.Sprintf("https://example.com/%s?ttl=%d", fullKey, int64(ttl.Seconds())), nil
	}
	return newWithClient(cfg, f, f, presign), f, calls
}

type presignCall struct {
	key string
	ttl time.Duration
}

func TestURL_ErrorsWhenPresignNotConfigured(t *testing.T) {
	// newStore() (via newWithClient with nil presign) is the test-only path.
	// Calling URL() in that mode must error rather than panic.
	store, _ := newStore(Config{})
	_, err := store.URL(context.Background(), "key", time.Hour)
	if err == nil {
		t.Fatal("expected error when presign is nil, got nil")
	}
	if !strings.Contains(err.Error(), "presign") {
		t.Errorf("error should mention presigning: %v", err)
	}
}

func TestURL_AppliesKeyPrefixAndForwardsTTL(t *testing.T) {
	store, _, calls := newStoreWithPresign(Config{KeyPrefix: "envs/prod"})
	got, err := store.URL(context.Background(), "uploads/file.pdf", 15*time.Minute)
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected 1 presign call, got %d", len(*calls))
	}
	if (*calls)[0].key != "envs/prod/uploads/file.pdf" {
		t.Errorf("presigner saw key %q, want %q (KeyPrefix must be applied)",
			(*calls)[0].key, "envs/prod/uploads/file.pdf")
	}
	if (*calls)[0].ttl != 15*time.Minute {
		t.Errorf("ttl forwarded as %v, want 15m", (*calls)[0].ttl)
	}
	if !strings.HasPrefix(got, "https://example.com/envs/prod/uploads/file.pdf") {
		t.Errorf("returned URL %q does not contain prefixed key", got)
	}
}

func TestURL_TTLZeroUsesAWSMaximum(t *testing.T) {
	// ttl=0 represents FileACLPublic — the adapter substitutes the AWS-cap of
	// 7 days so the URL is as long-lived as the SDK allows.
	store, _, calls := newStoreWithPresign(Config{})
	if _, err := store.URL(context.Background(), "k", 0); err != nil {
		t.Fatalf("URL: %v", err)
	}
	want := 7 * 24 * time.Hour
	if (*calls)[0].ttl != want {
		t.Errorf("ttl=0 produced presign ttl %v, want %v (AWS 7-day cap)", (*calls)[0].ttl, want)
	}
}

func TestURL_RejectsEmptyKey(t *testing.T) {
	store, _, _ := newStoreWithPresign(Config{})
	if _, err := store.URL(context.Background(), "", time.Hour); err == nil {
		t.Error("expected error for empty key")
	}
}

func TestURL_RejectsLeadingSlash(t *testing.T) {
	store, _, _ := newStoreWithPresign(Config{})
	if _, err := store.URL(context.Background(), "/leading", time.Hour); err == nil {
		t.Error("expected error for leading-slash key")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	// Compile-time assertion exists in s3.go; this is a runtime guard so
	// changes to maniflex.FileStorage break this test rather than every consumer.
	var s maniflex.FileStorage = (*S3Storage)(nil)
	_ = s
}

// ── helpers ──────────────────────────────────────────────────────────────────

func keysOf(m map[string]fakeObject) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// strings is imported but only used in this comment — drop the import if you
// remove the comment. (Keeping a reference to placate goimports.)
var _ = strings.TrimSpace

// awsv2 reference to keep import alive when tests don't otherwise use it.
var _ = awsv2.String
