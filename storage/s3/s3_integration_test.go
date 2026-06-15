//go:build integration

// Integration tests against a real S3 API. Skipped unless all of these env
// vars are set (so CI without credentials still passes):
//
//	S3_TEST_BUCKET       — bucket name (must already exist)
//	S3_TEST_REGION       — e.g. us-east-1
//	S3_TEST_ENDPOINT     — optional; set for MinIO/R2
//	S3_TEST_ACCESS_KEY   — optional; falls back to default credential chain
//	S3_TEST_SECRET_KEY   — optional
//	S3_TEST_PATH_STYLE   — "1" to force path-style (MinIO)
//
// Run with:
//
//	go test -tags=integration -run TestS3Integration ./storage/s3/...
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"testing"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/xaleel/maniflex"
)

func newIntegrationStore(t *testing.T) *S3Storage {
	t.Helper()

	bucket := os.Getenv("S3_TEST_BUCKET")
	region := os.Getenv("S3_TEST_REGION")
	if bucket == "" || region == "" {
		t.Skip("S3 integration: set S3_TEST_BUCKET and S3_TEST_REGION to run")
	}

	cfg := Config{
		Bucket:       bucket,
		Region:       region,
		Endpoint:     os.Getenv("S3_TEST_ENDPOINT"),
		UsePathStyle: os.Getenv("S3_TEST_PATH_STYLE") == "1",
		KeyPrefix:    "mfx-test/",
	}
	if ak := os.Getenv("S3_TEST_ACCESS_KEY"); ak != "" {
		awsCfg := awsv2.Config{
			Region:      region,
			Credentials: credentials.NewStaticCredentialsProvider(ak, os.Getenv("S3_TEST_SECRET_KEY"), ""),
		}
		cfg.AWSConfig = &awsCfg
	}

	store, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func TestS3Integration_RoundTrip(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()
	key := "round-trip.txt"
	content := []byte("hello s3 integration")

	t.Cleanup(func() { _ = store.Delete(ctx, key) })

	if err := store.Store(ctx, key, bytes.NewReader(content), maniflex.FileMeta{
		ContentType: "text/plain",
		Size:        int64(len(content)),
		Filename:    "round-trip.txt",
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	rc, meta, err := store.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch")
	}
	if meta.Filename != "round-trip.txt" {
		t.Errorf("Filename: got %q want round-trip.txt", meta.Filename)
	}
}

func TestS3Integration_LargeBodyUsesMultipart(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()
	key := "large.bin"

	t.Cleanup(func() { _ = store.Delete(ctx, key) })

	// 10 MB random payload — large enough to push the SDK's default 5 MB
	// part-size threshold into multipart territory.
	body := make([]byte, 10*1024*1024)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand: %v", err)
	}

	if err := store.Store(ctx, key, bytes.NewReader(body), maniflex.FileMeta{
		ContentType: "application/octet-stream",
		Size:        int64(len(body)),
		Filename:    "large.bin",
	}); err != nil {
		t.Fatalf("Store large: %v", err)
	}

	rc, _, err := store.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve large: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("large body content mismatch")
	}
}

func TestS3Integration_NotFound(t *testing.T) {
	store := newIntegrationStore(t)
	if _, _, err := store.Retrieve(context.Background(), "does/not/exist.bin"); !errors.Is(err, maniflex.ErrFileNotFound) {
		t.Errorf("err = %v, want maniflex.ErrFileNotFound", err)
	}
}

func TestS3Integration_Exists(t *testing.T) {
	store := newIntegrationStore(t)
	ctx := context.Background()

	if exists, _ := store.Exists(ctx, "absent.bin"); exists {
		t.Error("exists should be false for absent key")
	}
	key := "exists.bin"
	t.Cleanup(func() { _ = store.Delete(ctx, key) })

	store.Store(ctx, key, bytes.NewReader([]byte("x")), maniflex.FileMeta{Size: 1})
	if exists, err := store.Exists(ctx, key); err != nil || !exists {
		t.Errorf("after Store: exists=%v err=%v", exists, err)
	}
}

func TestS3Integration_DeleteIsIdempotent(t *testing.T) {
	store := newIntegrationStore(t)
	if err := store.Delete(context.Background(), "never-stored.bin"); err != nil {
		t.Errorf("delete on missing key returned error: %v", err)
	}
}
