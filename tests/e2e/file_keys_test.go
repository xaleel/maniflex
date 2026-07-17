package e2e

// R10 — maniflex.FileKeys: an mfx:"file" column holding many storage keys.
// Every rule a single-key file field enforces must bind per key; before this,
// a file array was skipped by all four file paths in silence.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func galleryServer(t *testing.T) (*testutil.Server, *testutil.MemoryStorage) {
	t.Helper()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{testutil.Gallery{}},
		FileStorage: store,
	})
	return srv, store
}

// putImage stages an object in storage and returns its key.
func putImage(t *testing.T, store *testutil.MemoryStorage, name string, size int, ct string) string {
	t.Helper()
	key := "uploads/" + name
	testutil.PutObject(t, store, key, ct, make([]byte, size))
	return key
}

// keysOf pulls a string array out of a response field.
func fileKeyArray(t *testing.T, data map[string]any, field string) []string {
	t.Helper()
	raw, ok := data[field].([]any)
	if !ok {
		t.Fatalf("field %q is not an array: %#v", field, data[field])
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

// ── round trip ──────────────────────────────────────────────────────────────

func TestFileKeys_RoundTripsAndPreservesOrder(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "a.jpg", 10, "image/jpeg")
	b := putImage(t, store, "b.jpg", 10, "image/jpeg")
	c := putImage(t, store, "c.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{
		"title": "g", "images": []any{c, a, b}, // deliberately not sorted
	})
	resp.AssertStatus(http.StatusCreated)

	read := srv.GET("/galleries/"+resp.ID(), nil)
	read.AssertStatus(http.StatusOK)
	got := fileKeyArray(t, read.Data(), "images")
	if len(got) != 3 {
		t.Fatalf("images: got %d keys, want 3", len(got))
	}
	// file_acl:signed rewrites each key to a URL, so compare the suffixes.
	for i, want := range []string{c, a, b} {
		if !strings.HasSuffix(got[i], want) {
			t.Errorf("position %d: got %q, want it to carry %q — a JSON array column "+
				"preserves the order written (a gallery is a sequence)", i, got[i], want)
		}
	}
}

func TestFileKeys_EmptyArrayIsStored(t *testing.T) {
	t.Parallel()
	srv, _ := galleryServer(t)
	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{}})
	resp.AssertStatus(http.StatusCreated)

	read := srv.GET("/galleries/"+resp.ID(), nil)
	if got := fileKeyArray(t, read.Data(), "images"); len(got) != 0 {
		t.Errorf("images: got %v, want []", got)
	}
}

// ── the rules bind per key ──────────────────────────────────────────────────

// The headline: a dangling key in the array is caught, exactly as a single-key
// field catches one. Before this the whole array was skipped in silence.
func TestFileKeys_DanglingKeyIsRejected(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	good := putImage(t, store, "good.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{
		"title": "g", "images": []any{good, "uploads/does-not-exist.jpg"},
	})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "FILE_NOT_FOUND" {
		t.Errorf("error code: got %q, want FILE_NOT_FOUND", code)
	}
}

// max_size binds per object, not to the array's total: ten 1MB images are ten
// valid files, not one 10MB one.
func TestFileKeys_MaxSizeBindsPerKey(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	ok1 := putImage(t, store, "small1.jpg", 900_000, "image/jpeg")
	ok2 := putImage(t, store, "small2.jpg", 900_000, "image/jpeg")
	big := putImage(t, store, "big.jpg", 2_000_000, "image/jpeg") // over max_size:1MB

	// Two under-limit files whose total exceeds 1MB: allowed.
	srv.POST("/galleries", map[string]any{"title": "g", "images": []any{ok1, ok2}}).
		AssertStatus(http.StatusCreated)

	// One over-limit file: refused.
	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{ok1, big}})
	resp.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := resp.ErrorCode(); code != "FILE_TOO_LARGE" {
		t.Errorf("error code: got %q, want FILE_TOO_LARGE", code)
	}
}

func TestFileKeys_AcceptBindsPerKey(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	img := putImage(t, store, "ok.jpg", 10, "image/jpeg")
	pdf := putImage(t, store, "no.pdf", 10, "application/pdf") // accept:image/*

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{img, pdf}})
	resp.AssertStatus(http.StatusUnsupportedMediaType)
	if code := resp.ErrorCode(); code != "FILE_TYPE_NOT_ALLOWED" {
		t.Errorf("error code: got %q, want FILE_TYPE_NOT_ALLOWED", code)
	}
}

func TestFileKeys_ACLSignsEveryKey(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "a.jpg", 10, "image/jpeg")
	b := putImage(t, store, "b.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{a, b}})
	resp.AssertStatus(http.StatusCreated)
	read := srv.GET("/galleries/"+resp.ID(), nil)

	for i, v := range fileKeyArray(t, read.Data(), "images") {
		if !strings.HasPrefix(v, "/files/") {
			t.Errorf("images[%d] = %q — file_acl:signed must rewrite every key, not just the first", i, v)
		}
	}
}

// ── the count cap ───────────────────────────────────────────────────────────

func TestFileKeys_MaxCountTagIsEnforced(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	var keys []any
	for i := range 3 {
		keys = append(keys, putImage(t, store, fmt.Sprintf("at%d.bin", i), 10, "application/octet-stream"))
	}

	// Attachments is max_count:2
	resp := srv.POST("/galleries", map[string]any{"title": "g", "attachments": keys})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "TOO_MANY_FILES" {
		t.Errorf("error code: got %q, want TOO_MANY_FILES", code)
	}

	srv.POST("/galleries", map[string]any{"title": "g", "attachments": keys[:2]}).
		AssertStatus(http.StatusCreated)
}

// An untagged field still has a ceiling — every key costs a Stat, so uncapped
// is one request buying N round-trips.
func TestFileKeys_DefaultCountCapApplies(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	img := putImage(t, store, "x.jpg", 10, "image/jpeg")

	over := make([]any, maniflex.DefaultMaxFileCount+1)
	for i := range over {
		over[i] = img
	}
	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": over})
	resp.AssertStatus(http.StatusUnprocessableEntity)
	if code := resp.ErrorCode(); code != "TOO_MANY_FILES" {
		t.Errorf("error code: got %q, want TOO_MANY_FILES", code)
	}
}

// ── auto_delete set-diff GC ─────────────────────────────────────────────────

func TestFileKeys_AutoDeleteGCsOnlyDroppedKeys(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "keep.jpg", 10, "image/jpeg")
	b := putImage(t, store, "drop.jpg", 10, "image/jpeg")
	c := putImage(t, store, "new.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{a, b}})
	resp.AssertStatus(http.StatusCreated)

	// Replace {a,b} with {a,c}: b is dropped, a is kept.
	srv.PATCH("/galleries/"+resp.ID(), map[string]any{"images": []any{a, c}}, nil).
		AssertStatus(http.StatusOK)

	waitFor(t, func() bool { return !store.HasKey(b) })
	if !store.HasKey(a) {
		t.Error("a key still referenced after the write must not be deleted")
	}
	if store.HasKey(b) {
		t.Error("a key dropped by the write must be GC'd")
	}
	if !store.HasKey(c) {
		t.Error("a newly added key must not be deleted")
	}
}

// Reordering drops nothing — the diff is a set difference, not positional.
func TestFileKeys_ReorderDeletesNothing(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "r1.jpg", 10, "image/jpeg")
	b := putImage(t, store, "r2.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{a, b}})
	resp.AssertStatus(http.StatusCreated)

	srv.PATCH("/galleries/"+resp.ID(), map[string]any{"images": []any{b, a}}, nil).
		AssertStatus(http.StatusOK)

	if !store.HasKey(a) || !store.HasKey(b) {
		t.Error("reordering a gallery must delete nothing — the diff is by set, not position")
	}
}

func TestFileKeys_AutoDeleteFalseKeepsDroppedKeys(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "n1.bin", 10, "application/octet-stream")
	b := putImage(t, store, "n2.bin", 10, "application/octet-stream")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "attachments": []any{a, b}})
	resp.AssertStatus(http.StatusCreated)
	srv.PATCH("/galleries/"+resp.ID(), map[string]any{"attachments": []any{a}}, nil).
		AssertStatus(http.StatusOK)

	if !store.HasKey(b) {
		t.Error("auto_delete:false must keep a dropped key")
	}
}

// Hard delete must clean up every key of every auto_delete list, not one.
func TestFileKeys_HardDeleteCleansEveryKey(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "d1.jpg", 10, "image/jpeg")
	b := putImage(t, store, "d2.jpg", 10, "image/jpeg")
	c := putImage(t, store, "d3.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{a, b, c}})
	resp.AssertStatus(http.StatusCreated)

	srv.DELETE("/galleries/"+resp.ID(), nil).AssertStatus(http.StatusNoContent)

	waitFor(t, func() bool { return !store.HasKey(a) && !store.HasKey(b) && !store.HasKey(c) })
	for _, k := range []string{a, b, c} {
		if store.HasKey(k) {
			t.Errorf("key %q survived a hard delete — cleanup keyed by column name could "+
				"only ever carry one key per field and leaked the rest", k)
		}
	}
}

// ── shapes that must be refused ─────────────────────────────────────────────

func TestFileKeys_MultipartUploadIsRefused(t *testing.T) {
	t.Parallel()
	srv, _ := galleryServer(t)

	resp := srv.POSTMultipart("/galleries", map[string]string{"title": "g"},
		map[string]testutil.FileUpload{
			"images": {Filename: "a.jpg", ContentType: "image/jpeg", Body: []byte("x")},
		})
	// Multipart carries one file per field, so letting this through would write
	// a single key into an array column and drop the other parts.
	resp.AssertStatus(http.StatusUnprocessableEntity)
}

func TestFileKeys_ScalarValueIsRejected(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "s.jpg", 10, "image/jpeg")

	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": a}) // string, not array
	resp.AssertStatus(http.StatusUnprocessableEntity)
}

// No attachment route is mounted for a key list — it streams one object and a
// list names no single one.
func TestFileKeys_NoAttachmentRouteMounted(t *testing.T) {
	t.Parallel()
	srv, store := galleryServer(t)
	a := putImage(t, store, "att.jpg", 10, "image/jpeg")
	resp := srv.POST("/galleries", map[string]any{"title": "g", "images": []any{a}})
	resp.AssertStatus(http.StatusCreated)

	srv.GET("/galleries/"+resp.ID()+"/images", nil).AssertStatus(http.StatusNotFound)
}

// ── OpenAPI ─────────────────────────────────────────────────────────────────

func TestFileKeys_AppearsInSpecAsAnArray(t *testing.T) {
	t.Parallel()
	srv, _ := galleryServer(t)
	spec := srv.GET("/openapi.json", nil)
	spec.AssertStatus(http.StatusOK)

	props := schemaProps(t, spec, "Gallery")
	images, ok := props["images"].(map[string]any)
	if !ok {
		t.Fatal("FileKeys field absent from the spec — goTypeToSchema maps no slice kind, " +
			"so without ObjectWithSchema the column would be invisible to a generated client")
	}
	if images["type"] != "array" {
		t.Errorf("images type: got %v, want array", images["type"])
	}
	items, _ := images["items"].(map[string]any)
	if items == nil || items["type"] != "string" {
		t.Errorf("images items: got %v, want {type: string}", images["items"])
	}
}
