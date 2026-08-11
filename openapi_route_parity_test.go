package maniflex

// Route/spec parity. Every route mountModel registers must be described in the
// generated OpenAPI document, or a client generated from the spec cannot reach
// it and a reader of the spec concludes it does not exist.
//
// The gap this test closes was real: /export, /aggregate, /{id}/restore and
// /{field}/upload-url were all mounted and none of them appeared in the spec,
// while the docs asserted the aggregate endpoint did. Spot-checking four paths
// would have fixed those four; walking the router catches the fifth.

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// ── Test fixtures ─────────────────────────────────────────────────────────────

// parityStorage is the minimum FileStorage that lets storage-gated routes mount.
// Nothing in this test issues a request, so every method is a stub.
type parityStorage struct{}

func (parityStorage) Store(context.Context, string, io.Reader, FileMeta) error { return nil }
func (parityStorage) Retrieve(context.Context, string) (io.ReadCloser, FileMeta, error) {
	return nil, FileMeta{}, ErrFileNotFound
}
func (parityStorage) Delete(context.Context, string) error         { return nil }
func (parityStorage) Exists(context.Context, string) (bool, error) { return false, nil }
func (parityStorage) Stat(context.Context, string) (FileMeta, error) {
	return FileMeta{}, ErrFileNotFound
}
func (parityStorage) PresignUpload(context.Context, string, PresignUploadOptions) (*PresignedUpload, error) {
	return &PresignedUpload{}, nil
}
func (parityStorage) URL(context.Context, string, PresignURLOptions) (string, error) {
	return "", nil
}

// parityParcel opts into every conditionally-mounted model route at once:
// export, aggregate, restore (which needs soft-delete), version history, a
// downloadable file field, and a presigned-upload field.
type parityParcel struct {
	BaseModel
	WithDeletedAt
	Status  string `json:"status" db:"status" mfx:"filterable,sortable"`
	Weight  int    `json:"weight" db:"weight" mfx:"filterable"`
	Label   string `json:"label" db:"label" mfx:"file"`
	Scanned string `json:"scanned" db:"scanned" mfx:"file,upload:presigned"`
}

// parityServer builds a sealed server whose every optional model route is
// mounted, plus the standalone file endpoints.
func parityServer(t *testing.T) *Server {
	t.Helper()
	s := New(Config{
		PathPrefix: "/api",
		FilesConfig: FilesConfig{
			Storage:        parityStorage{},
			MountEndpoints: true,
		},
	})
	if err := s.Register(parityParcel{}, ModelConfig{
		TableName:        "parcels",
		ExportEnabled:    true,
		AggregateEnabled: true,
		RestoreEnabled:   true,
		Versioned:        true,
	}); err != nil {
		t.Fatalf("registering parityParcel: %v", err)
	}
	return s
}

// ── Router walking ────────────────────────────────────────────────────────────

// routeKey is one endpoint reduced to what parity is actually about: the
// method and the *shape* of the path. display keeps the original spelling for
// error messages; path is the normalised form the comparison uses.
type routeKey struct {
	method  string
	path    string
	display string
}

// key is what parity compares on. display is deliberately excluded: two
// spellings of the same shape are the same endpoint.
func (r routeKey) key() string { return r.method + " " + r.path }

func (r routeKey) String() string { return strings.ToUpper(r.method) + " " + r.display }

// normalisePath collapses every parameter segment to a placeholder, so parity
// compares structure rather than the names two independent pieces of code
// happened to choose. chi writes a trailing catch-all as "*" where the spec
// names it ("/files/*" ↔ "/files/{key}"), and nothing requires chi's "{id}" to
// be spelled "{id}" in the document either — but a literal segment must stay
// literal, or "/parcels/export" would match "/parcels/{id}".
func normalisePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "*" || (strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}")) {
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// specExemptPaths are routes that exist outside the documented API surface.
// Probes and the documentation endpoints themselves are infrastructure, not
// resources — an OpenAPI document that described its own URL would be odd, and
// the probes are deliberately outside the pipeline the spec models.
var specExemptPaths = map[string]bool{
	"/live":          true,
	"/ready":         true,
	"/health":        true,
	"/openapi.json":  true,
	"/asyncapi.json": true,
}

// walkMountedRoutes returns every documented-surface route the router mounts.
// HEAD and OPTIONS are excluded: they are auto-mounted for every model and
// carry no schema a client would generate against.
func walkMountedRoutes(t *testing.T, s *Server) []routeKey {
	t.Helper()

	routes, ok := s.Handler().(chi.Routes)
	if !ok {
		t.Fatal("Server.Handler() is not a chi.Routes — cannot walk mounted routes")
	}

	var out []routeKey
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		method = strings.ToLower(method)
		if method == "head" || method == "options" {
			return nil
		}
		path := strings.TrimPrefix(route, s.cfg.PathPrefix)
		// chi renders a sub-router's own root as a trailing slash ("/parcels/");
		// the spec writes it bare.
		if len(path) > 1 {
			path = strings.TrimSuffix(path, "/")
		}
		if specExemptPaths[path] {
			return nil
		}
		out = append(out, routeKey{
			method:  method,
			path:    normalisePath(path),
			display: path,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking routes: %v", err)
	}
	return out
}

// specRoutes returns every operation the generated document describes.
func specRoutes(t *testing.T, s *Server) []routeKey {
	t.Helper()

	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)
	var out []routeKey
	for path, item := range spec.Paths {
		for method, op := range map[string]*OASOperation{
			"get": item.Get, "post": item.Post, "patch": item.Patch,
			"put": item.Put, "delete": item.Delete,
		} {
			if op != nil {
				out = append(out, routeKey{
					method:  method,
					path:    normalisePath(path),
					display: path,
				})
			}
		}
	}
	return out
}

// routeKeySet indexes routes by their comparison key.
func routeKeySet(routes []routeKey) map[string]bool {
	out := make(map[string]bool, len(routes))
	for _, r := range routes {
		out[r.key()] = true
	}
	return out
}

// ── The parity test ───────────────────────────────────────────────────────────

// TestOpenAPIRouteParity fails when a mounted route is missing from the spec.
// It is a drift guard: a new conditionally-mounted route added to mountModel
// without a matching buildModelPaths branch shows up here by name.
func TestOpenAPIRouteParity(t *testing.T) {
	s := parityServer(t)
	documented := routeKeySet(specRoutes(t, s))

	var missing []string
	for _, r := range walkMountedRoutes(t, s) {
		if !documented[r.key()] {
			missing = append(missing, r.String())
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("%d mounted route(s) absent from the OpenAPI spec:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestOpenAPISpecDescribesNoPhantomRoutes is parity in the other direction: a
// documented path that nothing serves sends a generated client to a 404.
func TestOpenAPISpecDescribesNoPhantomRoutes(t *testing.T) {
	s := parityServer(t)

	mounted := routeKeySet(walkMountedRoutes(t, s))

	var phantom []string
	for _, r := range specRoutes(t, s) {
		if !mounted[r.key()] {
			phantom = append(phantom, r.String())
		}
	}
	sort.Strings(phantom)

	if len(phantom) > 0 {
		t.Errorf("%d documented route(s) that nothing mounts:\n  %s",
			len(phantom), strings.Join(phantom, "\n  "))
	}
}

// ── Per-route documentation ───────────────────────────────────────────────────

// The parity test proves the paths exist. These prove they carry the details a
// client needs, which parity alone cannot tell apart from an empty stub.

func TestOpenAPIExportPathIsDocumented(t *testing.T) {
	s := parityServer(t)
	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)

	item, ok := spec.Paths["/parcels/export"]
	if !ok {
		t.Fatal("spec has no /parcels/export path")
	}
	if item.Get == nil {
		t.Fatal("/parcels/export has no GET operation")
	}
	if !hasParam(item.Get.Parameters, "format") {
		t.Error("GET /parcels/export does not document the ?format= parameter")
	}
	if !hasParam(item.Get.Parameters, "filter") {
		t.Error("GET /parcels/export does not document ?filter=, which it accepts")
	}
	if _, ok := item.Get.Responses["200"]; !ok {
		t.Error("GET /parcels/export documents no 200 response")
	}
	if ct := item.Get.Responses["200"].Content; ct != nil {
		if _, ok := ct["text/csv"]; !ok {
			t.Error("GET /parcels/export 200 does not offer text/csv")
		}
	}
}

func TestOpenAPIAggregatePathIsDocumented(t *testing.T) {
	s := parityServer(t)
	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)

	item, ok := spec.Paths["/parcels/aggregate"]
	if !ok {
		t.Fatal("spec has no /parcels/aggregate path")
	}
	if item.Get == nil {
		t.Fatal("/parcels/aggregate has no GET operation")
	}
	if !hasParam(item.Get.Parameters, "aggregate") {
		t.Fatal("GET /parcels/aggregate does not document the ?aggregate= parameter, " +
			"which is the only way to describe the aggregation")
	}
	// The spec must be sent in the query string, not a body — that is the whole
	// reason the endpoint is shaped this way.
	if item.Get.RequestBody != nil {
		t.Error("GET /parcels/aggregate documents a request body; the endpoint refuses one")
	}
	desc := paramByName(item.Get.Parameters, "aggregate").Description
	for _, want := range []string{"select", "group_by", "url-encoded"} {
		if !strings.Contains(strings.ToLower(desc), want) {
			t.Errorf("?aggregate= description does not mention %q: %s", want, desc)
		}
	}
	if _, ok := item.Get.Responses["400"]; !ok {
		t.Error("GET /parcels/aggregate documents no 400 response")
	}
}

func TestOpenAPIRestorePathIsDocumented(t *testing.T) {
	s := parityServer(t)
	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)

	item, ok := spec.Paths["/parcels/{id}/restore"]
	if !ok {
		t.Fatal("spec has no /parcels/{id}/restore path")
	}
	if item.Post == nil {
		t.Fatal("/parcels/{id}/restore has no POST operation")
	}
	if item.Post.RequestBody != nil {
		t.Error("POST /parcels/{id}/restore documents a request body; it carries none")
	}
	if !hasParam(item.Post.Parameters, "id") {
		t.Error("POST /parcels/{id}/restore does not document the id path parameter")
	}
	if _, ok := item.Post.Responses["404"]; !ok {
		t.Error("POST /parcels/{id}/restore documents no 404 — restoring a live row is one")
	}
}

func TestOpenAPIPresignUploadPathIsDocumented(t *testing.T) {
	s := parityServer(t)
	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)

	item, ok := spec.Paths["/parcels/scanned/upload-url"]
	if !ok {
		t.Fatal("spec has no /parcels/scanned/upload-url path")
	}
	if item.Post == nil {
		t.Fatal("/parcels/scanned/upload-url has no POST operation")
	}
	if item.Post.RequestBody == nil {
		t.Fatal("POST /parcels/scanned/upload-url documents no request body")
	}
	body := item.Post.RequestBody.Content["application/json"].Schema
	if body == nil {
		t.Fatal("POST /parcels/scanned/upload-url has no application/json request schema")
	}
	for _, field := range []string{"filename", "content_type", "size"} {
		if _, ok := body.Properties[field]; !ok {
			t.Errorf("presign request schema is missing %q", field)
		}
	}
	// A field without upload:presigned mounts no such route.
	if _, ok := spec.Paths["/parcels/label/upload-url"]; ok {
		t.Error("spec describes an upload-url route for a field that does not opt into it")
	}
}

// TestOpenAPIOptInRoutesAreAbsentWhenNotEnabled is the other half of every
// opt-in above: documenting an endpoint a model did not enable would send a
// client to a 404 just as surely as omitting one it did.
func TestOpenAPIOptInRoutesAreAbsentWhenNotEnabled(t *testing.T) {
	type plainParcel struct {
		BaseModel
		Status string `json:"status" db:"status" mfx:"filterable"`
	}
	s := New(Config{PathPrefix: "/api"})
	if err := s.Register(plainParcel{}, ModelConfig{TableName: "plains"}); err != nil {
		t.Fatalf("registering plainParcel: %v", err)
	}
	spec := GenerateSpec(s.registry, &s.cfg, s.actions, s.globalSearch)

	for _, path := range []string{
		"/plains/export",
		"/plains/aggregate",
		"/plains/{id}/restore",
	} {
		if _, ok := spec.Paths[path]; ok {
			t.Errorf("spec describes %s on a model that did not opt into it", path)
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func hasParam(params []OASParameter, name string) bool {
	for _, p := range params {
		if p.Name == name {
			return true
		}
	}
	return false
}

func paramByName(params []OASParameter, name string) OASParameter {
	for _, p := range params {
		if p.Name == name {
			return p
		}
	}
	return OASParameter{}
}
