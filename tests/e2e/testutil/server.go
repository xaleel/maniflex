// Package testutil provides shared helpers for the e2e test suite.
// Every test gets its own isolated in-memory SQLite server that is torn down
// when the test ends.
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
)

// Server wraps an httptest.Server with convenience helpers.
type Server struct {
	*httptest.Server
	t      testing.TB
	prefix string
	mfx    *maniflex.Server
}

// ManiflexServer returns the underlying maniflex.Server. Use it to obtain the
// DB adapter and registry for constructing a background ServerContext in tests:
//
//	bgCtx := maniflex.NewBackground(context.Background(), srv.ManiflexServer().DB(), srv.ManiflexServer().Registry())
func (s *Server) ManiflexServer() *maniflex.Server { return s.mfx }

// Options configures the test server.
type Options struct {
	// Models to register. Defaults to the standard test fixtures if empty.
	Models []any
	// Middleware is called after models are registered; use it to wire up
	// pipeline middleware for specific test scenarios.
	Middleware func(s *maniflex.Server)
	// PathPrefix overrides the default "/api" prefix.
	PathPrefix string
	// AutoMigrate defaults to true when not set.
	AutoMigrate *bool
	// DB path
	DBPath      string
	PanicLogger *slog.Logger
	// Logger sets Config.Logger — the logger the pipeline logs through. Supply a
	// buffer-backed handler to assert on what the framework does (or doesn't) log.
	Logger *slog.Logger
	// Trace sets Config.Trace — pipeline tracing. Notably, Trace.Steps wraps the
	// transaction in maniflex's tracedTx, so it exercises a different Tx type.
	Trace maniflex.PipelineTrace
	// QueryTimeout sets Config.QueryTimeout on the Server instance.
	// Zero (the default) means no per-request timeout.
	QueryTimeout time.Duration
	// HealthCheckDB sets Config.HealthCheckDB — enables DB ping on /health.
	HealthCheckDB bool
	// HealthTimeout sets Config.HealthTimeout. Only used when HealthCheckDB is true.
	HealthTimeout time.Duration
	// DBAdapter overrides the SQLite adapter with a custom one. When non-nil
	// DBPath is ignored. The function receives the registry and must return
	// a fully-initialised adapter (AutoMigrate is called on it separately).
	DBAdapter func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error)
	// FileStorage sets the file storage backend. When non-nil, file upload
	// endpoints and model file-field handling are enabled.
	FileStorage maniflex.FileStorage
	// FileMiddleware wraps the standalone /files endpoints.
	FileMiddleware []maniflex.MiddlewareFunc
	// FilesConfig, when non-nil, is used verbatim as maniflex.Config.FilesConfig,
	// overriding the FileStorage/FileMiddleware convenience fields above. Use it
	// to exercise the full FilesConfig surface (KeyGen, MountEndpoints,
	// AfterMiddlewares) that the convenience fields don't reach.
	FilesConfig *maniflex.FilesConfig
	// KeyProvider sets the encryption key provider for mfx:"encrypted" fields.
	KeyProvider maniflex.KeyProvider
	// TrustProxyHeaders sets Config.TrustProxyHeaders — when true, chi's RealIP
	// derives RemoteAddr from X-Forwarded-For / X-Real-IP. Off by default.
	TrustProxyHeaders bool
}

// NewServer creates a fully-initialised Maniflex server backed by an in-memory
// SQLite database and returns it as an httptest.Server.
// The server and database are automatically closed when t ends.
func NewServer(t testing.TB, opts Options) *Server {
	t.Helper()

	prefix := opts.PathPrefix
	if prefix == "" {
		prefix = "/api"
	}

	autoMigrate := true
	if opts.AutoMigrate != nil {
		autoMigrate = *opts.AutoMigrate
	}

	models := opts.Models
	if len(models) == 0 {
		models = DefaultModels()
	}

	filesConfig := maniflex.FilesConfig{
		Storage:           opts.FileStorage,
		MountEndpoints:    opts.FileStorage != nil,
		BeforeMiddlewares: opts.FileMiddleware,
	}
	if opts.FilesConfig != nil {
		filesConfig = *opts.FilesConfig
	}

	server := maniflex.New(maniflex.Config{
		PathPrefix:         prefix,
		DisableAutoMigrate: !autoMigrate,
		PanicLogger:        opts.PanicLogger,
		Logger:             opts.Logger,
		Trace:              opts.Trace,
		QueryTimeout:       opts.QueryTimeout,
		HealthCheckDB:      opts.HealthCheckDB,
		HealthTimeout:      opts.HealthTimeout,
		FilesConfig:        filesConfig,
		KeyProvider:        opts.KeyProvider,
		TrustProxyHeaders:  opts.TrustProxyHeaders,
	})
	server.MustRegister(models...)

	var db maniflex.DBAdapter
	switch {
	case opts.DBAdapter != nil:
		var err error
		db, err = opts.DBAdapter(server.Registry())
		if err != nil {
			t.Fatalf("testutil: open custom adapter: %v", err)
		}
	case IsPostgres():
		// Postgres lane: an isolated per-test schema, dropped in t.Cleanup.
		db = openPostgres(t, server.Registry())
	default:
		// SQLite lane (default): a fresh in-memory database per test.
		DBPath := opts.DBPath
		if DBPath == "" {
			DBPath = ":memory:"
		}
		var err error
		db, err = sqlite.Open(DBPath, server.Registry())
		if err != nil {
			t.Fatalf("testutil: open sqlite: %v", err)
		}
	}

	server.SetDB(db)

	// Trigger auto-migration by calling a throwaway migration directly —
	// we use the adapter's AutoMigrate so we don't have to start a real HTTP
	// server (Handler() does NOT call AutoMigrate; only Start() does).
	if autoMigrate {
		if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
			t.Fatalf("testutil: auto-migrate: %v", err)
		}
	}

	if opts.Middleware != nil {
		opts.Middleware(server)
	}

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		ts.Close()
		db.Close()
	})

	return &Server{Server: ts, t: t, prefix: prefix, mfx: server}
}

// URL returns the full URL for the given path relative to the server root.
// Leading slash in path is optional.
func (s *Server) URL(path string) string {
	if len(path) > 0 && path[0] == '/' {
		return s.Server.URL + path
	}
	return s.Server.URL + "/" + path
}

// APIPath returns the full URL for a path relative to the API prefix.
func (s *Server) APIPath(path string) string {
	if len(path) > 0 && path[0] == '/' {
		return s.Server.URL + s.prefix + path
	}
	return s.Server.URL + s.prefix + "/" + path
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

// Do performs an HTTP request and returns the response and parsed body.
// body may be:
//   - nil            → no body
//   - []byte         → sent verbatim (useful for raw/malformed JSON tests)
//   - io.Reader      → streamed directly
//   - any other type → JSON-encoded
func (s *Server) Do(method, url string, body any, headers ...map[string]string) *Response {
	s.t.Helper()

	var bodyReader io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			bodyReader = bytes.NewReader(v)
		case io.Reader:
			bodyReader = v
		default:
			b, err := json.Marshal(v)
			if err != nil {
				s.t.Fatalf("testutil: marshal body: %v", err)
			}
			bodyReader = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		s.t.Fatalf("testutil: new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, hMap := range headers {
		for k, v := range hMap {
			req.Header.Set(k, v)
		}
	}

	resp, err := s.Server.Client().Do(req)
	if err != nil {
		s.t.Fatalf("testutil: do request: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testutil: read body: %v", err)
	}

	return &Response{t: s.t, Status: resp.StatusCode, Body: raw, Header: resp.Header}
}

func (s *Server) GET(path string, headers ...map[string]string) *Response {
	return s.Do(http.MethodGet, s.APIPath(path), nil, headers...)
}
func (s *Server) POST(path string, body any, headers ...map[string]string) *Response {
	return s.Do(http.MethodPost, s.APIPath(path), body, headers...)
}
func (s *Server) PATCH(path string, body any, headers ...map[string]string) *Response {
	return s.Do(http.MethodPatch, s.APIPath(path), body, headers...)
}
func (s *Server) DELETE(path string, headers ...map[string]string) *Response {
	return s.Do(http.MethodDelete, s.APIPath(path), nil, headers...)
}

// ── Response ──────────────────────────────────────────────────────────────────

// Response wraps an HTTP response with assertion helpers.
type Response struct {
	t      testing.TB
	Status int
	Body   []byte
	Header http.Header
}

// AssertStatus fails the test if the status code does not match.
func (r *Response) AssertStatus(want int) *Response {
	r.t.Helper()
	if r.Status != want {
		r.t.Errorf("status: got %d, want %d\nbody: %s", r.Status, want, r.Body)
	}
	return r
}

// AssertJSON parses the body as JSON and calls check with the result.
func (r *Response) AssertJSON(check func(body map[string]any)) *Response {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("parse JSON body: %v\nbody: %s", err, r.Body)
	}
	check(m)
	return r
}

// Data extracts the "data" key from the response body as a map.
// Fails the test if the body is not parseable or "data" is missing.
func (r *Response) Data() map[string]any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("parse response body: %v\nbody: %s", err, r.Body)
	}
	d, ok := m["data"]
	if !ok {
		r.t.Fatalf("response has no 'data' key\nbody: %s", r.Body)
	}
	dm, ok := d.(map[string]any)
	if !ok {
		r.t.Fatalf("'data' is not an object (got %T)\nbody: %s", d, r.Body)
	}
	return dm
}

// DataList extracts the "data" key from the response body as a slice.
func (r *Response) DataList() []any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("parse response body: %v\nbody: %s", err, r.Body)
	}
	d, ok := m["data"]
	if !ok {
		r.t.Fatalf("response has no 'data' key\nbody: %s", r.Body)
	}
	dl, ok := d.([]any)
	if !ok {
		r.t.Fatalf("'data' is not an array (got %T)\nbody: %s", d, r.Body)
	}
	return dl
}

// Meta extracts the "meta" pagination block.
func (r *Response) Meta() map[string]any {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("parse response body: %v\nbody: %s", err, r.Body)
	}
	meta, ok := m["meta"].(map[string]any)
	if !ok {
		r.t.Fatalf("response has no 'meta' object\nbody: %s", r.Body)
	}
	return meta
}

// ErrorCode extracts the error.code from an error response.
func (r *Response) ErrorCode() string {
	r.t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		r.t.Fatalf("parse response body: %v\nbody: %s", err, r.Body)
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		r.t.Fatalf("response has no 'error' object\nbody: %s", r.Body)
	}
	code, _ := errObj["code"].(string)
	return code
}

// ID extracts the "id" field from the data object.
func (r *Response) ID() string {
	r.t.Helper()
	return r.Data()["id"].(string)
}

// ── Seed helpers ──────────────────────────────────────────────────────────────

// CreateUser creates a User record and returns the response.
func (s *Server) CreateUser(name, email, role string) *Response {
	return s.POST("/users", map[string]any{
		"name":     name,
		"email":    email,
		"password": "secret",
		"role":     role,
	})
}

// CreatePost creates a Post record and returns the response.
func (s *Server) CreatePost(title, status, userID string) *Response {
	return s.POST("/posts", map[string]any{
		"title":   title,
		"body":    fmt.Sprintf("Body of %s", title),
		"status":  status,
		"user_id": userID,
	})
}

// CreateComment creates a Comment record and returns the response.
func (s *Server) CreateComment(body, postID, userID string) *Response {
	return s.POST("/comments", map[string]any{
		"body":    body,
		"post_id": postID,
		"user_id": userID,
	})
}

// MustID creates a resource and fatals if it fails, returning the ID.
func (s *Server) MustID(resp *Response) string {
	s.t.Helper()
	resp.AssertStatus(http.StatusCreated)
	return resp.ID()
}

// POSTMultipart sends a multipart/form-data POST request.
// fields contains regular form values, files maps field name → FileUpload.
func (s *Server) POSTMultipart(path string, fields map[string]string, files map[string]FileUpload) *Response {
	s.t.Helper()
	return s.doMultipart(http.MethodPost, path, fields, files)
}

// PATCHMultipart sends a multipart/form-data PATCH request.
func (s *Server) PATCHMultipart(path string, fields map[string]string, files map[string]FileUpload) *Response {
	s.t.Helper()
	return s.doMultipart(http.MethodPatch, path, fields, files)
}

// FileUpload describes a file to include in a multipart request.
type FileUpload struct {
	Filename    string
	ContentType string
	Body        []byte
}

func (s *Server) doMultipart(method, path string, fields map[string]string, files map[string]FileUpload) *Response {
	s.t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			s.t.Fatalf("testutil: write field %q: %v", k, err)
		}
	}
	for fieldName, fu := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{
			fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, fu.Filename),
		}
		if fu.ContentType != "" {
			h["Content-Type"] = []string{fu.ContentType}
		}
		part, err := w.CreatePart(h)
		if err != nil {
			s.t.Fatalf("testutil: create part %q: %v", fieldName, err)
		}
		if _, err := part.Write(fu.Body); err != nil {
			s.t.Fatalf("testutil: write part %q: %v", fieldName, err)
		}
	}
	w.Close()

	req, err := http.NewRequest(method, s.APIPath(path), &buf)
	if err != nil {
		s.t.Fatalf("testutil: new request: %v", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := s.Server.Client().Do(req)
	if err != nil {
		s.t.Fatalf("testutil: do request: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testutil: read body: %v", err)
	}

	return &Response{t: s.t, Status: resp.StatusCode, Body: raw, Header: resp.Header}
}

// GETRaw performs a raw GET and returns the response (status, headers, body bytes)
// without JSON parsing. Useful for file-serving tests.
func (s *Server) GETRaw(path string) *Response {
	s.t.Helper()
	resp, err := s.Server.Client().Get(s.APIPath(path))
	if err != nil {
		s.t.Fatalf("testutil: get: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("testutil: read body: %v", err)
	}
	return &Response{t: s.t, Status: resp.StatusCode, Body: raw, Header: resp.Header}
}

// ── MemoryStorage ────────────────────────────────────────────────────────────
// In-memory FileStorage implementation for tests. Avoids filesystem access and
// allows introspection of stored files.

// MemoryStorage is a thread-safe, in-memory maniflex.FileStorage for tests.
type MemoryStorage struct {
	mu    sync.RWMutex
	files map[string]memFile
}

type memFile struct {
	data []byte
	meta maniflex.FileMeta
}

// NewMemoryStorage returns a ready-to-use in-memory storage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{files: make(map[string]memFile)}
}

func (m *MemoryStorage) Store(_ context.Context, key string, r io.Reader, meta maniflex.FileMeta) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta.Size = int64(len(data))
	m.files[key] = memFile{data: data, meta: meta}
	return nil
}

func (m *MemoryStorage) Retrieve(_ context.Context, key string) (io.ReadCloser, maniflex.FileMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.files[key]
	if !ok {
		return nil, maniflex.FileMeta{}, maniflex.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(f.data)), f.meta, nil
}

func (m *MemoryStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[key]; !ok {
		return maniflex.ErrFileNotFound
	}
	delete(m.files, key)
	return nil
}

func (m *MemoryStorage) Exists(_ context.Context, key string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.files[key]
	return ok, nil
}

// URL implements maniflex.FileStorage. Returns a server-relative /files/<key> path
// regardless of ttl — MemoryStorage has no presigning capability.
func (m *MemoryStorage) URL(_ context.Context, key string, _ time.Duration) (string, error) {
	if key == "" {
		return "", fmt.Errorf("storage: key must not be empty")
	}
	return "/files/" + key, nil
}

// HasKey reports whether the given key exists in storage. Test-only helper.
func (m *MemoryStorage) HasKey(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.files[key]
	return ok
}

// Keys returns all stored keys. Test-only helper.
func (m *MemoryStorage) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.files))
	for k := range m.files {
		keys = append(keys, k)
	}
	return keys
}
