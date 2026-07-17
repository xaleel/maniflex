package e2e

// P1-20 — a record could reference *any* existing storage key, including another
// record's or another tenant's. The reference check proved only that the object
// existed, never that the caller was allowed to use it, so a leaked key (they
// travel in signed URLs and file_acl:private responses) could be pinned onto the
// attacker's own record — and an auto_delete field could then delete a blob its
// record never owned.
//
// The fix binds a minted key to the principal that mints it: the key carries a
// hash of the caller's scope, and a reference by a different principal is refused
// with 403 FILE_FORBIDDEN. A key with no scope marker (minted before the fix, or
// for an anonymous caller) still passes to the existence check, so the change is
// non-breaking. These tests exercise all three minting paths (POST /files, a
// presigned upload, and a multipart upload through a model) and both reference
// paths (a single-key field and a FileKeys list).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── fixtures ──────────────────────────────────────────────────────────────────

type Vault struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
	File  string `json:"file" db:"file" mfx:"file"`
}

type Album struct {
	maniflex.BaseModel
	Title  string            `json:"title" db:"title"`
	Images maniflex.FileKeys `json:"images" db:"images" mfx:"file"`
}

type Reel struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
	Clip  string `json:"clip" db:"clip" mfx:"file,upload:presigned,accept:video/mp4,max_size:100"`
}

// Locker's Doc field is auto_delete — a plain mfx:"file" already defaults
// AutoDelete to true, which is exactly the destructive capability P1-20 abuses.
type Locker struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title"`
	Doc   string `json:"doc" db:"doc" mfx:"file"`
}

// tenantAuthMW is a minimal auth middleware: it reads X-Tenant and populates
// ctx.Auth, so a test can act as distinct principals. The default key scope
// derives from ctx.Auth.TenantID, so this is all it takes for two tenants to
// mint keys in separate scopes.
func tenantAuthMW(ctx *maniflex.ServerContext, next func() error) error {
	if tid := ctx.Request.Header.Get("X-Tenant"); tid != "" {
		ctx.Auth = &maniflex.AuthInfo{UserID: "u-" + tid, TenantID: tid}
	}
	return next()
}

func asTenant(tid string) map[string]string { return map[string]string{"X-Tenant": tid} }

func scopeServer(t *testing.T) (*testutil.Server, *testutil.MemoryStorage) {
	t.Helper()
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{Vault{}, Album{}, Reel{}, Locker{}},
		FileStorage: store,
		// The standalone POST /files mint must see the tenant too.
		FileMiddleware: []maniflex.MiddlewareFunc{tenantAuthMW},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(tenantAuthMW)
		},
	})
	return srv, store
}

// mintKey uploads a file via POST /files as the given tenant and returns the
// minted (scoped) storage key.
func mintKey(t *testing.T, srv *testutil.Server, tenant, filename, ct string, size int) string {
	t.Helper()
	res := srv.POSTMultipart("/files", nil,
		map[string]testutil.FileUpload{
			"file": {Filename: filename, ContentType: ct, Body: make([]byte, size)},
		},
		asTenant(tenant))
	res.AssertStatus(http.StatusCreated)
	key, _ := res.Data()["key"].(string)
	if key == "" {
		t.Fatalf("mint: response carried no key: %s", res.Body)
	}
	return key
}

// ── the core fix: a leaked key cannot be referenced by another principal ───────

func TestFileScope_CrossPrincipalKeyReferenceRefused(t *testing.T) {
	srv, _ := scopeServer(t)
	keyA := mintKey(t, srv, "A", "a.txt", "text/plain", 10)

	if !strings.HasPrefix(keyA, "mfxs1/") {
		t.Fatalf("a key minted under a principal should be scoped, got %q", keyA)
	}

	// Tenant B knows A's key (it leaks) and tries to pin it onto B's own record.
	// Before the fix this was a 201; the record then pointed at A's blob.
	res := srv.POST("/vaults", map[string]any{"title": "b", "file": keyA}, asTenant("B"))
	res.AssertStatus(http.StatusForbidden)
	if code := res.ErrorCode(); code != "FILE_FORBIDDEN" {
		t.Errorf("code = %q, want FILE_FORBIDDEN", code)
	}

	// The minter, tenant A, references its own key without trouble.
	srv.POST("/vaults", map[string]any{"title": "a", "file": keyA}, asTenant("A")).
		AssertStatus(http.StatusCreated)
}

func TestFileScope_FileKeysArrayRefusesForeignKey(t *testing.T) {
	srv, _ := scopeServer(t)
	keyA := mintKey(t, srv, "A", "a.jpg", "image/jpeg", 10)

	// One foreign key anywhere in the array taints the whole write.
	own := mintKey(t, srv, "B", "b.jpg", "image/jpeg", 10)
	srv.POST("/albums", map[string]any{"title": "b", "images": []any{own, keyA}}, asTenant("B")).
		AssertStatus(http.StatusForbidden)

	// B's gallery of only its own keys is fine.
	srv.POST("/albums", map[string]any{"title": "b", "images": []any{own}}, asTenant("B")).
		AssertStatus(http.StatusCreated)

	// A's gallery of A's key is fine.
	srv.POST("/albums", map[string]any{"title": "a", "images": []any{keyA}}, asTenant("A")).
		AssertStatus(http.StatusCreated)
}

// ── the destructive half: a foreign key can't be adopted, so auto_delete on the
// attacker's record can never reach the victim's blob ──────────────────────────

func TestFileScope_ForeignKeyCannotBeAdoptedSoAutoDeleteCannotCrossTenants(t *testing.T) {
	srv, store := scopeServer(t)
	keyA := mintKey(t, srv, "A", "a.txt", "text/plain", 10)

	// A legitimately owns a record pointing at its key.
	srv.POST("/lockers", map[string]any{"title": "a", "doc": keyA}, asTenant("A")).
		AssertStatus(http.StatusCreated)

	// B cannot adopt A's key onto B's own auto_delete record — so B can never
	// trigger deletion of A's blob by replacing it later.
	srv.POST("/lockers", map[string]any{"title": "b", "doc": keyA}, asTenant("B")).
		AssertStatus(http.StatusForbidden)

	if !store.HasKey(keyA) {
		t.Fatalf("A's blob %q was deleted — the cross-tenant adoption was not prevented", keyA)
	}
}

// ── every minting path binds ───────────────────────────────────────────────────

func TestFileScope_PresignedMintIsScoped(t *testing.T) {
	srv, store := scopeServer(t)

	res := srv.POST("/reels/clip/upload-url",
		map[string]any{"filename": "c.mp4", "content_type": "video/mp4", "size": 10},
		asTenant("A")).AssertStatus(http.StatusOK)
	key, _ := res.Data()["key"].(string)
	if !strings.HasPrefix(key, "mfxs1/") {
		t.Fatalf("a presigned key minted under a principal should be scoped, got %q", key)
	}
	// The client would upload straight to the bucket; simulate the stored object
	// so the owner's completing write finds it.
	testutil.PutObject(t, store, key, "video/mp4", make([]byte, 10))

	srv.POST("/reels", map[string]any{"title": "b", "clip": key}, asTenant("B")).
		AssertStatus(http.StatusForbidden)
	srv.POST("/reels", map[string]any{"title": "a", "clip": key}, asTenant("A")).
		AssertStatus(http.StatusCreated)
}

func TestFileScope_MultipartModelUploadIsScoped(t *testing.T) {
	srv, _ := scopeServer(t)

	// A uploads a file straight to a model field (multipart through the pipeline).
	created := srv.POSTMultipart("/vaults",
		map[string]string{"title": "a"},
		map[string]testutil.FileUpload{
			"file": {Filename: "a.txt", ContentType: "text/plain", Body: make([]byte, 10)},
		},
		asTenant("A"))
	created.AssertStatus(http.StatusCreated)

	// The stored key is bound to A (a plain file field returns the raw key).
	read := srv.GET("/vaults/"+created.ID(), asTenant("A")).AssertStatus(http.StatusOK)
	key, _ := read.Data()["file"].(string)
	if !strings.HasPrefix(key, "mfxs1/") {
		t.Fatalf("a model-uploaded key should be scoped, got %q", key)
	}

	// B cannot adopt A's uploaded key.
	srv.POST("/vaults", map[string]any{"title": "b", "file": key}, asTenant("B")).
		AssertStatus(http.StatusForbidden)
}

// ── non-breaking: keys that carry no scope marker still work ────────────────────

func TestFileScope_LegacyUnscopedKeyStillReferenceable(t *testing.T) {
	srv, store := scopeServer(t)

	// A key with no scope marker — as if stored before this release.
	legacy := "uploads/legacy/doc.txt"
	testutil.PutObject(t, store, legacy, "text/plain", make([]byte, 10))

	srv.POST("/vaults", map[string]any{"title": "x", "file": legacy}, asTenant("B")).
		AssertStatus(http.StatusCreated)
}

func TestFileScope_AnonymousUploadIsUnscopedAndReferenceable(t *testing.T) {
	srv, _ := scopeServer(t)

	// No X-Tenant → no principal → nothing to bind to → an unscoped key.
	key := mintKey(t, srv, "", "anon.txt", "text/plain", 10)
	if strings.HasPrefix(key, "mfxs1/") {
		t.Fatalf("an anonymous upload has no principal to bind to, but got a scoped key %q", key)
	}

	// An unscoped key is referenceable by anyone — the documented residual gap,
	// and what keeps the change non-breaking.
	srv.POST("/vaults", map[string]any{"title": "b", "file": key}, asTenant("B")).
		AssertStatus(http.StatusCreated)
}

// ── the scope is configurable ──────────────────────────────────────────────────

func TestFileScope_KeyScopeHookOverridesDefault(t *testing.T) {
	store := testutil.NewMemoryStorage()
	// Scope by a custom header instead of ctx.Auth.
	byTeam := func(ctx *maniflex.ServerContext) string { return ctx.Request.Header.Get("X-Team") }
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Vault{}},
		FilesConfig: &maniflex.FilesConfig{
			Storage:        store,
			MountEndpoints: true,
			KeyScope:       byTeam,
		},
	})

	res := srv.POSTMultipart("/files", nil,
		map[string]testutil.FileUpload{
			"file": {Filename: "f.txt", ContentType: "text/plain", Body: make([]byte, 10)},
		},
		map[string]string{"X-Team": "red"})
	res.AssertStatus(http.StatusCreated)
	key, _ := res.Data()["key"].(string)
	if !strings.HasPrefix(key, "mfxs1/") {
		t.Fatalf("custom KeyScope should still produce a scoped key, got %q", key)
	}

	srv.POST("/vaults", map[string]any{"title": "b", "file": key}, map[string]string{"X-Team": "blue"}).
		AssertStatus(http.StatusForbidden)
	srv.POST("/vaults", map[string]any{"title": "r", "file": key}, map[string]string{"X-Team": "red"}).
		AssertStatus(http.StatusCreated)
}
