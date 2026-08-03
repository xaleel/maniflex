package e2e_test

// MS-3 — how a file field is exposed, and who may write it.
//
// Two halves, found together.
//
// The first is the ask: suppress the raw storage key from responses while
// keeping the attachment route mounted. That already exists and is spelled
// mfx:"writeonly" — the client uploads, the key never appears in a response, and
// GET /{model}/{id}/{field} still streams the bytes. These tests pin it so it
// cannot regress, since nothing covered the combination before.
//
// The second is a hole the first uncovered. Readonly is enforced by stripping
// the field from ctx.ParsedBody, but a multipart upload arrives in ctx.Files and
// never passes through that strip — so mfx:"file,readonly" (and mfx:"file,hidden",
// which implies it) accepted a client upload on create AND let a client overwrite
// the stored object on update. A server-managed document — a generated report, a
// countersigned contract — was client-writable through the multipart door, with
// the tag saying otherwise.
//
//	go test ./e2e/ -run TestFileWritability

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type FWDoc struct {
	maniflex.BaseModel
	Title string `json:"title"  db:"title"`
	// The MS-3 shape: uploadable, never echoed, still downloadable.
	Secret string `json:"secret" db:"secret" mfx:"file,writeonly"`
	// Server-managed: the client may neither set nor replace it.
	Report string `json:"report" db:"report" mfx:"file,readonly"`
	// Hidden implies readonly, and additionally never appears in a response.
	Audit string `json:"audit" db:"audit" mfx:"file,hidden"`
	// Set once, never replaced.
	Contract string `json:"contract" db:"contract" mfx:"file,immutable"`
}

func fwServer(t *testing.T) (*testutil.Server, *maniflex.ServerContext) {
	t.Helper()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{FWDoc{}},
		FileStorage: testutil.NewMemoryStorage(),
	})
	return srv, maniflex.NewBackground(t.Context(),
		srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
}

func upload(body string) testutil.FileUpload {
	return testutil.FileUpload{
		Filename: "f.txt", ContentType: "text/plain", Body: []byte(body),
	}
}

func storedCol(t *testing.T, ctx *maniflex.ServerContext, id, col string) string {
	t.Helper()
	row, err := ctx.GetModel("FWDoc").Read(id)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s, _ := row[col].(string)
	return s
}

// ── The MS-3 shape ───────────────────────────────────────────────────────────

// writeonly is the answer to "hide the key but keep the routes": the upload
// works, the key is absent from every response, and the bytes are still
// reachable through the attachment route.
func TestFileWritability_WriteOnlyHidesTheKeyAndKeepsTheRoute(t *testing.T) {
	t.Parallel()
	srv, ctx := fwServer(t)

	resp := srv.POSTMultipart("/fw_docs", map[string]string{"title": "t"},
		map[string]testutil.FileUpload{"secret": upload("secret bytes")})
	resp.AssertStatus(http.StatusCreated)
	id := resp.Data()["id"].(string)

	// Stored, so the upload was not merely discarded.
	if storedCol(t, ctx, id, "secret") == "" {
		t.Fatal("a writeonly file field must still accept an upload")
	}
	// Absent from create, read and list.
	for name, body := range map[string][]byte{
		"create": resp.Body,
		"read":   srv.GET("/fw_docs/" + id).Body,
		"list":   srv.GET("/fw_docs").Body,
	} {
		if strings.Contains(string(body), `"secret"`) {
			t.Errorf("%s response leaks the storage key: %s", name, body)
		}
	}
	// And still downloadable.
	att := srv.GETRaw("/fw_docs/" + id + "/secret")
	if att.Status != http.StatusOK {
		t.Fatalf("attachment route: %d, want 200", att.Status)
	}
	if string(att.Body) != "secret bytes" {
		t.Errorf("attachment body = %q", att.Body)
	}
}

// A generated client has to know the field is send-only rather than missing, or
// it will model the response as carrying a key that never arrives.
func TestFileWritability_WriteOnlyIsDeclaredInOpenAPI(t *testing.T) {
	t.Parallel()
	srv, _ := fwServer(t)
	spec := string(srv.GET("/openapi.json").Body)

	if !strings.Contains(spec, `"secret":{"type":"string","description":"Storage key for an uploaded file","writeOnly":true}`) {
		t.Error(`the read schema must mark "secret" writeOnly`)
	}
	if !strings.Contains(spec, "/fw_docs/{id}/secret") {
		t.Error("the attachment route must still be documented")
	}
}

// ── The hole ─────────────────────────────────────────────────────────────────

// readonly must mean readonly on every door, not just the JSON one.
func TestFileWritability_ReadonlyRefusesAnUpload(t *testing.T) {
	t.Parallel()
	srv, _ := fwServer(t)

	resp := srv.POSTMultipart("/fw_docs", map[string]string{"title": "t"},
		map[string]testutil.FileUpload{"report": upload("client bytes")})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422 — a readonly file field is not client-writable\n%s",
			resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "report") {
		t.Errorf("the error must name the field: %s", resp.Body)
	}
}

// The update case is the sharper one: the object already exists and belongs to
// the server, and this let a client replace its bytes.
func TestFileWritability_ReadonlyCannotBeOverwrittenOnUpdate(t *testing.T) {
	t.Parallel()
	srv, ctx := fwServer(t)

	created := srv.POST("/fw_docs", map[string]any{"title": "t"})
	created.AssertStatus(http.StatusCreated)
	id := created.Data()["id"].(string)

	// The server sets it, as a readonly field is meant to be set.
	if _, err := ctx.GetModel("FWDoc").Update(id, map[string]any{"report": "server/report.pdf"}); err != nil {
		t.Fatalf("server-side write: %v", err)
	}

	resp := srv.PATCHMultipart("/fw_docs/"+id, map[string]string{},
		map[string]testutil.FileUpload{"report": upload("overwritten")})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422\n%s", resp.Status, resp.Body)
	}
	if got := storedCol(t, ctx, id, "report"); got != "server/report.pdf" {
		t.Errorf("the stored key changed to %q — a client replaced a server-managed file", got)
	}
}

// hidden implies readonly, so it inherits the refusal.
func TestFileWritability_HiddenRefusesAnUpload(t *testing.T) {
	t.Parallel()
	srv, _ := fwServer(t)

	resp := srv.POSTMultipart("/fw_docs", map[string]string{"title": "t"},
		map[string]testutil.FileUpload{"audit": upload("client bytes")})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422\n%s", resp.Status, resp.Body)
	}
}

// immutable is set-once: the create upload lands, the replacement does not.
func TestFileWritability_ImmutableAcceptsOnceThenRefuses(t *testing.T) {
	t.Parallel()
	srv, ctx := fwServer(t)

	created := srv.POSTMultipart("/fw_docs", map[string]string{"title": "t"},
		map[string]testutil.FileUpload{"contract": upload("original")})
	created.AssertStatus(http.StatusCreated)
	id := created.Data()["id"].(string)
	first := storedCol(t, ctx, id, "contract")
	if first == "" {
		t.Fatal("an immutable file field must accept its first upload")
	}

	resp := srv.PATCHMultipart("/fw_docs/"+id, map[string]string{},
		map[string]testutil.FileUpload{"contract": upload("replacement")})
	if resp.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422\n%s", resp.Status, resp.Body)
	}
	if got := storedCol(t, ctx, id, "contract"); got != first {
		t.Errorf("the immutable key changed from %q to %q", first, got)
	}
}

// The spec must not advertise what the server refuses. A generated client that
// offers a "report" upload part produces a request that can only ever 422.
func TestFileWritability_SpecDoesNotOfferUnwritableParts(t *testing.T) {
	t.Parallel()
	srv, _ := fwServer(t)
	spec := string(srv.GET("/openapi.json").Body)

	create := schemaBlock(t, spec, "FWDocCreateMultipart")
	for _, banned := range []string{"report", "audit"} {
		if strings.Contains(create, `"`+banned+`"`) {
			t.Errorf("the create multipart schema offers %q, which the server refuses:\n%s",
				banned, create)
		}
	}
	for _, wanted := range []string{"secret", "contract"} {
		if !strings.Contains(create, `"`+wanted+`"`) {
			t.Errorf("the create multipart schema is missing %q:\n%s", wanted, create)
		}
	}

	update := schemaBlock(t, spec, "FWDocUpdateMultipart")
	for _, banned := range []string{"report", "audit", "contract"} {
		if strings.Contains(update, `"`+banned+`"`) {
			t.Errorf("the update multipart schema offers %q, which the server refuses:\n%s",
				banned, update)
		}
	}
	if !strings.Contains(update, `"secret"`) {
		t.Errorf("the update multipart schema is missing %q:\n%s", "secret", update)
	}
}

// schemaBlock returns the JSON object declared for a named component schema.
func schemaBlock(t *testing.T, spec, name string) string {
	t.Helper()
	i := strings.Index(spec, `"`+name+`":`)
	if i < 0 {
		t.Fatalf("schema %q is not in the spec", name)
	}
	rest := spec[i+len(name)+3:]
	depth := 0
	for j, c := range rest {
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:j+1]
			}
		}
	}
	t.Fatalf("schema %q is not balanced", name)
	return ""
}
