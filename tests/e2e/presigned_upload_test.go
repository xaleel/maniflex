package e2e

// R5 — mfx:"file,upload:presigned" mints a one-shot authorisation so a client
// uploads straight to storage, and the record then names only the key.
//
// Two halves, and the second one already existed:
//
//   - Minting (new): POST /{model}/{field}/upload-url. No record id, because a
//     create-time file field has no record yet — the shape the request filed
//     (POST /{model}/{id}/{field}/upload-url) could not serve its own example.
//   - Completion (already shipped): the ordinary write naming the key, which the
//     framework existence-checks. No pending-state table is needed because this
//     step already exists.
//
// What did NOT exist was any enforcement on that second half: it checked only
// that the key existed, so uploading out of band and then referencing the key
// walked past max_size and accept entirely. Presigned upload makes that path the
// normal one for the largest files in the system, so these tests spend most of
// their time there rather than on the minting.

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Clip has a presigned video field with both rules set, so every test can ask
// whether the rules bind on whichever path the bytes took.
type Clip struct {
	maniflex.BaseModel
	Title string `json:"title" db:"title" mfx:"required"`
	Video string `json:"video" db:"video" mfx:"file,upload:presigned,accept:video/mp4,max_size:100"`
}

func clipSrv(t *testing.T) (*testutil.Server, *testutil.MemoryStorage) {
	t.Helper()
	store := testutil.NewMemoryStorage()
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{Clip{}},
		FileStorage: store,
	}), store
}

// ── minting ──────────────────────────────────────────────────────────────────

func TestPresign_MintsAnUploadAuthorisation(t *testing.T) {
	srv, _ := clipSrv(t)

	res := srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mp4", "content_type": "video/mp4", "size": 50,
	}).AssertStatus(http.StatusOK)

	got := res.Data()
	for _, k := range []string{"url", "method", "key", "expires_at"} {
		if got[k] == nil || got[k] == "" {
			t.Errorf("minted upload is missing %q: %v", k, got)
		}
	}
	if got["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST — a POST policy is what pins the size", got["method"])
	}
	// The cap is echoed so a client can refuse a too-large file before spending
	// the upload rather than after.
	if got["max_size"] != float64(100) {
		t.Errorf("max_size = %v, want 100", got["max_size"])
	}
}

// The client never chooses the key: one that could would aim its upload at
// another record's object.
func TestPresign_KeyIsMintedByTheServer(t *testing.T) {
	srv, _ := clipSrv(t)

	res := srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mp4", "content_type": "video/mp4",
		"key": "someone-elses/object.mp4", // ignored: not part of the contract
	}).AssertStatus(http.StatusOK)

	if got := res.Data()["key"]; got == "someone-elses/object.mp4" {
		t.Fatalf("the server returned the client's key %v — a client can aim an upload "+
			"at an arbitrary object", got)
	}
}

// The field's rules are checked before anything is minted. A URL issued for a
// file the field would refuse can only ever store an object no record can name.
func TestPresign_RefusesBeforeMintingWhatTheFieldWouldReject(t *testing.T) {
	srv, _ := clipSrv(t)

	srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mov", "content_type": "video/quicktime", "size": 50,
	}).AssertStatus(http.StatusUnsupportedMediaType)

	srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mp4", "content_type": "video/mp4", "size": 5000,
	}).AssertStatus(http.StatusRequestEntityTooLarge)
}

func TestPresign_RequiresFilenameAndContentType(t *testing.T) {
	srv, _ := clipSrv(t)

	srv.POST("/clips/video/upload-url", map[string]any{"content_type": "video/mp4"}).
		AssertStatus(http.StatusUnprocessableEntity)
	srv.POST("/clips/video/upload-url", map[string]any{"filename": "clip.mp4"}).
		AssertStatus(http.StatusUnprocessableEntity)
}

// A backend that cannot presign must say so. The one thing it must never do is
// hand back an unsigned URL: that is not a degraded presigned upload, it is an
// open write endpoint. LocalStorage.URL sets that precedent on the read side and
// it must not be repeated here.
func TestPresign_UnsupportedBackendIs501NotAnUnsignedURL(t *testing.T) {
	store := testutil.NewMemoryStorage()
	store.PresignFails = true
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Clip{}}, FileStorage: store,
	})

	res := srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mp4", "content_type": "video/mp4",
	})
	res.AssertStatus(http.StatusNotImplemented)
	if code := res.ErrorCode(); code != "PRESIGN_UNSUPPORTED" {
		t.Errorf("error code = %q, want PRESIGN_UNSUPPORTED", code)
	}
}

// The route is opt-in: a plain mfx:"file" field mounts nothing.
func TestPresign_RouteIsOptIn(t *testing.T) {
	store := testutil.NewMemoryStorage()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{testutil.ACLDoc{}}, // file fields, none presigned
		FileStorage: store,
	})
	srv.POST("/acl_docs/raw_key/upload-url", map[string]any{
		"filename": "x.bin", "content_type": "application/octet-stream",
	}).AssertStatus(http.StatusNotFound)
}

// ── completion: the half that already existed and enforced nothing ───────────

// The headline fix. Before this, the same bytes the multipart path answered 413
// for were accepted with a 201 when referenced by key.
func TestPresign_CompletionEnforcesMaxSize(t *testing.T) {
	srv, store := clipSrv(t)

	// An object that is over the field's cap, as a client with a stolen or
	// mis-signed URL could leave behind.
	key := "uploads/x/big.mp4"
	testutil.PutObject(t, store, key, "video/mp4", make([]byte, 500))

	res := srv.POST("/clips", map[string]any{"title": "t", "video": key})
	res.AssertStatus(http.StatusRequestEntityTooLarge)
	if code := res.ErrorCode(); code != "FILE_TOO_LARGE" {
		t.Errorf("error code = %q, want FILE_TOO_LARGE", code)
	}
}

func TestPresign_CompletionEnforcesAccept(t *testing.T) {
	srv, store := clipSrv(t)

	key := "uploads/x/evil.exe"
	testutil.PutObject(t, store, key, "application/x-msdownload", []byte("tiny"))

	res := srv.POST("/clips", map[string]any{"title": "t", "video": key})
	res.AssertStatus(http.StatusUnsupportedMediaType)
	if code := res.ErrorCode(); code != "FILE_TYPE_NOT_ALLOWED" {
		t.Errorf("error code = %q, want FILE_TYPE_NOT_ALLOWED", code)
	}
}

// The two paths must give the same verdict on the same bytes — that is the whole
// reason the rule lives in one place now.
func TestPresign_BothPathsAgree(t *testing.T) {
	srv, store := clipSrv(t)

	over := make([]byte, 500)
	direct := srv.POSTMultipart("/clips", map[string]string{"title": "a"},
		map[string]testutil.FileUpload{
			"video": {Filename: "big.mp4", ContentType: "video/mp4", Body: over},
		})

	key := "uploads/x/big2.mp4"
	testutil.PutObject(t, store, key, "video/mp4", over)
	byKey := srv.POST("/clips", map[string]any{"title": "b", "video": key})

	if direct.Status != byKey.Status {
		t.Errorf("same bytes, different answers: multipart %d, key-reference %d — the two "+
			"paths disagree about the field's rules", direct.Status, byKey.Status)
	}
}

// An update must be bound too, or the rules hold only until the first PATCH.
func TestPresign_CompletionEnforcedOnUpdate(t *testing.T) {
	srv, store := clipSrv(t)

	good := "uploads/x/ok.mp4"
	testutil.PutObject(t, store, good, "video/mp4", []byte("small"))
	id := srv.MustID(srv.POST("/clips", map[string]any{"title": "t", "video": good}))

	bad := "uploads/x/huge.mp4"
	testutil.PutObject(t, store, bad, "video/mp4", make([]byte, 500))
	srv.PATCH("/clips/"+id, map[string]any{"video": bad}, nil).
		AssertStatus(http.StatusRequestEntityTooLarge)

	if got := srv.GET("/clips/" + id).Data()["video"]; got != good {
		t.Errorf("video = %v, want %q — the refused PATCH still wrote", got, good)
	}
}

// The other half: a legitimate two-phase upload must actually work end to end.
func TestPresign_HappyPathCompletes(t *testing.T) {
	srv, store := clipSrv(t)

	mint := srv.POST("/clips/video/upload-url", map[string]any{
		"filename": "clip.mp4", "content_type": "video/mp4", "size": 20,
	}).AssertStatus(http.StatusOK)
	key, _ := mint.Data()["key"].(string)

	// Stand in for the client PUT/POSTing straight to storage.
	testutil.PutObject(t, store, key, "video/mp4", []byte("twenty bytes exactly"))

	res := srv.POST("/clips", map[string]any{"title": "my clip", "video": key})
	res.AssertStatus(http.StatusCreated)
	if got := res.Data()["video"]; got != key {
		t.Errorf("video = %v, want the minted key %q", got, key)
	}
}

// A dangling key is still refused — the invariant that a record can never
// reference a key that is not there must survive the new path.
func TestPresign_DanglingKeyStillRefused(t *testing.T) {
	srv, _ := clipSrv(t)

	res := srv.POST("/clips", map[string]any{"title": "t", "video": "uploads/nope/gone.mp4"})
	res.AssertStatus(http.StatusUnprocessableEntity)
	if code := res.ErrorCode(); code != "FILE_NOT_FOUND" {
		t.Errorf("error code = %q, want FILE_NOT_FOUND", code)
	}
}

// A stored object whose type the backend never recorded cannot be checked against
// an accept list — so it is refused rather than waved through. An accept rule
// that cannot be evaluated is not satisfied.
func TestPresign_UnknownStoredTypeIsRefusedNotWaived(t *testing.T) {
	srv, store := clipSrv(t)

	key := "uploads/x/mystery.mp4"
	testutil.PutObject(t, store, key, "", []byte("small"))

	srv.POST("/clips", map[string]any{"title": "t", "video": key}).
		AssertStatus(http.StatusUnsupportedMediaType)
}
