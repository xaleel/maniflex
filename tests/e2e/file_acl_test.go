package e2e

// 3B.4 — mfx:"file_acl" directive. The Response step rewrites file field
// values according to the configured ACL mode:
//   - private (default): raw storage key (download via /files/<key> or attachment route)
//   - signed: FileStorage.URL(ctx, key, ttl)  → e.g. presigned URL
//   - public: FileStorage.URL(ctx, key, 0)    → permanent / long-lived URL
//
// MemoryStorage.URL returns "/files/<key>" for any ttl, so we can probe the
// signed/public branches without an actual presigner.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// aclSetup uploads three files via /files and creates an ACLDoc referencing
// each by key. Returns the response data from the create call.
func aclSetup(t *testing.T) (data map[string]any, server *testutil.Server, store *testutil.MemoryStorage, keys [3]string) {
	t.Helper()
	store = testutil.NewMemoryStorage()
	server = testutil.NewServer(t, testutil.Options{
		Models:      []any{testutil.ACLDoc{}},
		FileStorage: store,
	})

	files := []string{"private.bin", "signed.bin", "public.bin"}
	for i, name := range files {
		resp := server.POSTMultipart("/files", nil, map[string]testutil.FileUpload{
			"file": {Filename: name, ContentType: "application/octet-stream", Body: []byte("body-" + name)},
		})
		resp.AssertStatus(http.StatusCreated)
		keys[i] = testutil.Field(t, resp.Data(), "key")
	}

	createResp := server.POST("/acl_docs", map[string]any{
		"title":       "acl-doc",
		"raw_key":     keys[0],
		"signed_file": keys[1],
		"public_file": keys[2],
	})
	createResp.AssertStatus(http.StatusCreated)
	return createResp.Data(), server, store, keys
}

func TestFileACL_PrivateReturnsRawKey(t *testing.T) {
	t.Parallel()
	data, _, _, keys := aclSetup(t)
	rawKey := testutil.Field(t, data, "raw_key")
	if rawKey != keys[0] {
		t.Errorf("private mode: raw_key = %q, want raw storage key %q", rawKey, keys[0])
	}
}

func TestFileACL_SignedReturnsURL(t *testing.T) {
	t.Parallel()
	data, _, _, keys := aclSetup(t)
	signed := testutil.Field(t, data, "signed_file")
	if signed == keys[1] {
		t.Errorf("signed mode: value should be a URL, got raw key %q", signed)
	}
	if !strings.HasPrefix(signed, "/files/") {
		t.Errorf("MemoryStorage signed URL should be /files/<key>, got %q", signed)
	}
	if !strings.HasSuffix(signed, keys[1]) {
		t.Errorf("signed URL should reference the original key %q, got %q", keys[1], signed)
	}
}

func TestFileACL_PublicReturnsURL(t *testing.T) {
	t.Parallel()
	data, _, _, keys := aclSetup(t)
	pub := testutil.Field(t, data, "public_file")
	if pub == keys[2] {
		t.Errorf("public mode: value should be a URL, got raw key %q", pub)
	}
	if !strings.HasPrefix(pub, "/files/") {
		t.Errorf("MemoryStorage public URL should be /files/<key>, got %q", pub)
	}
}

func TestFileACL_AppliedOnRead(t *testing.T) {
	// The rewrite also applies on GET /:model/:id, not only on the create
	// response. Verify by re-reading the same record.
	t.Parallel()
	createData, srv, _, keys := aclSetup(t)
	id := testutil.Field(t, createData, "id")

	readResp := srv.GET("/acl_docs/"+id, nil)
	readResp.AssertStatus(http.StatusOK)
	data := readResp.Data()

	if v := testutil.Field(t, data, "raw_key"); v != keys[0] {
		t.Errorf("read: private mode broken, raw_key = %q, want %q", v, keys[0])
	}
	if v := testutil.Field(t, data, "signed_file"); v == keys[1] {
		t.Errorf("read: signed mode broken, still returning raw key %q", v)
	}
	if v := testutil.Field(t, data, "public_file"); v == keys[2] {
		t.Errorf("read: public mode broken, still returning raw key %q", v)
	}
}

func TestFileACL_AppliedOnList(t *testing.T) {
	// Same rewrite must run on list responses.
	t.Parallel()
	_, srv, _, keys := aclSetup(t)

	resp := srv.GET("/acl_docs", nil)
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) == 0 {
		t.Fatal("list returned no rows")
	}
	row, _ := rows[0].(map[string]any)
	if v, _ := row["signed_file"].(string); v == keys[1] {
		t.Errorf("list: signed mode broken, raw key %q leaked", v)
	}
	if v, _ := row["public_file"].(string); v == keys[2] {
		t.Errorf("list: public mode broken, raw key %q leaked", v)
	}
}

func TestFileACL_EmptyKeySkipped(t *testing.T) {
	// A record with no file value (empty string) should pass through unchanged
	// — rewriteFileACL skips empty keys so we don't fabricate URLs to nothing.
	t.Parallel()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{testutil.ACLDoc{}},
		FileStorage: store,
	})

	// All three file fields empty.
	resp := srv.POST("/acl_docs", map[string]any{
		"title":       "no-files",
		"raw_key":     "",
		"signed_file": "",
		"public_file": "",
	})
	resp.AssertStatus(http.StatusCreated)
	data := resp.Data()
	for _, k := range []string{"signed_file", "public_file"} {
		v, _ := data[k].(string)
		if v != "" {
			t.Errorf("%s should remain empty, got %q", k, v)
		}
	}
}
