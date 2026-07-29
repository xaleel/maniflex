// Package maniflextest provides a small, supported integration-test harness
// for applications built with Maniflex.
//
// New starts a real HTTP server, migrates the application's models into an
// isolated in-memory SQLite database by default, and registers cleanup with the
// test. Postgres provides schema-isolated PostgreSQL tests when database-specific
// behaviour matters.
package maniflextest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
)

// Options configures one test server.
type Options struct {
	// Config is passed to maniflex.New. Config.DB must be nil; use Database.
	Config maniflex.Config
	// Models are passed directly to Server.MustRegister, including any adjacent
	// ModelConfig values.
	Models []any
	// Setup registers application middleware, actions, services, and other
	// configuration after models and the database are available but before the
	// router is sealed and migrations run.
	Setup func(*maniflex.Server)
	// Database defaults to a distinct in-memory SQLite database.
	Database DatabaseFactory
	// StartServices runs lifecycle hooks and registered services.
	StartServices bool
	// DisableTestAuth omits the middleware used by As.
	DisableTestAuth bool
}

// Server wraps an httptest.Server with Maniflex-aware request helpers.
type Server struct {
	*httptest.Server
	t   testing.TB
	app *maniflex.Server
}

// New builds, migrates, and starts an isolated test server. All resources are
// released automatically through t.Cleanup.
func New(t testing.TB, opts Options) *Server {
	t.Helper()

	if opts.Config.DB != nil {
		t.Fatal("maniflextest: Options.Config.DB must be nil; use Options.Database")
	}
	if opts.Config.ShutdownTimeout == 0 {
		opts.Config.ShutdownTimeout = 5 * time.Second
	}
	shutdownTimeout := opts.Config.ShutdownTimeout

	app := maniflex.New(opts.Config)
	if len(opts.Models) > 0 {
		app.MustRegister(opts.Models...)
	}

	factory := opts.Database
	if factory == nil {
		factory = SQLite()
	}
	database, err := factory(context.Background(), app.Registry())
	if err != nil {
		t.Fatalf("maniflextest: open database: %v", err)
	}
	if database.Adapter == nil {
		t.Fatal("maniflextest: database factory returned a nil adapter")
	}
	app.SetDB(database.Adapter)

	var httpServer *httptest.Server
	t.Cleanup(func() {
		if httpServer != nil {
			httpServer.Close()
		}
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := app.Shutdown(shutdownCtx); err != nil {
			t.Errorf("maniflextest: shut down application: %v", err)
		}
		cancelShutdown()

		if database.Cleanup != nil {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			if err := database.Cleanup(cleanupCtx); err != nil {
				t.Errorf("maniflextest: clean up database: %v", err)
			}
			cancelCleanup()
		}
		if err := database.Adapter.Close(); err != nil {
			t.Errorf("maniflextest: close database: %v", err)
		}
	})

	if !opts.DisableTestAuth {
		app.Pipeline.Auth.Register(testAuth, maniflex.WithName("maniflextest-principal"))
	}
	if opts.Setup != nil {
		opts.Setup(app)
	}
	if err := app.MigrateOnly(context.Background()); err != nil {
		t.Fatalf("maniflextest: migrate application: %v", err)
	}
	if opts.StartServices {
		if err := app.StartServices(); err != nil {
			t.Fatalf("maniflextest: start services: %v", err)
		}
	}

	httpServer = httptest.NewServer(app.Handler())
	return &Server{Server: httpServer, t: t, app: app}
}

// App returns the underlying application for background operations and
// assertions that do not go through HTTP.
func (s *Server) App() *maniflex.Server { return s.app }

// URL resolves path at the HTTP server root.
func (s *Server) URL(path string) string {
	return joinURL(s.Server.URL, path)
}

// APIURL resolves path beneath the application's configured PathPrefix.
func (s *Server) APIURL(path string) string {
	return joinURL(s.Server.URL, s.app.PathPrefix(), path)
}

func joinURL(parts ...string) string {
	out := strings.TrimRight(parts[0], "/")
	for _, part := range parts[1:] {
		out += "/" + strings.Trim(part, "/")
	}
	return out
}

// RequestOption changes an outgoing harness request.
type RequestOption func(*http.Request) error

// Header adds one request header.
func Header(name, value string) RequestOption {
	return func(req *http.Request) error {
		req.Header.Set(name, value)
		return nil
	}
}

// Bearer adds an Authorization bearer token.
func Bearer(token string) RequestOption {
	return Header("Authorization", "Bearer "+token)
}

// Do sends an API-relative request. nil sends no body; an io.Reader or []byte is
// sent as-is; every other value is JSON encoded.
func (s *Server) Do(method, path string, body any, opts ...RequestOption) *Response {
	s.t.Helper()
	return s.do(method, s.APIURL(path), body, opts...)
}

// DoRoot sends a request relative to the HTTP server root, for endpoints such
// as standalone file routes. Generated routes, including health, use Do.
func (s *Server) DoRoot(method, path string, body any, opts ...RequestOption) *Response {
	s.t.Helper()
	return s.do(method, s.URL(path), body, opts...)
}

func (s *Server) do(method, url string, body any, opts ...RequestOption) *Response {
	s.t.Helper()

	var reader io.Reader
	contentType := ""
	switch value := body.(type) {
	case nil:
	case io.Reader:
		reader = value
	case []byte:
		reader = bytes.NewReader(value)
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			s.t.Fatalf("maniflextest: encode request body: %v", err)
		}
		reader = bytes.NewReader(raw)
		contentType = "application/json"
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		s.t.Fatalf("maniflextest: build request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(req); err != nil {
			s.t.Fatalf("maniflextest: apply request option: %v", err)
		}
	}

	res, err := s.Client().Do(req)
	if err != nil {
		s.t.Fatalf("maniflextest: send request: %v", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		s.t.Fatalf("maniflextest: read response: %v", err)
	}
	return &Response{
		t:          s.t,
		StatusCode: res.StatusCode,
		Header:     res.Header.Clone(),
		Body:       raw,
	}
}

func (s *Server) GET(path string, opts ...RequestOption) *Response {
	return s.Do(http.MethodGet, path, nil, opts...)
}

func (s *Server) POST(path string, body any, opts ...RequestOption) *Response {
	return s.Do(http.MethodPost, path, body, opts...)
}

func (s *Server) PUT(path string, body any, opts ...RequestOption) *Response {
	return s.Do(http.MethodPut, path, body, opts...)
}

func (s *Server) PATCH(path string, body any, opts ...RequestOption) *Response {
	return s.Do(http.MethodPatch, path, body, opts...)
}

func (s *Server) DELETE(path string, opts ...RequestOption) *Response {
	return s.Do(http.MethodDelete, path, nil, opts...)
}

// Response is the fully buffered result of a harness request.
type Response struct {
	t          testing.TB
	StatusCode int
	Header     http.Header
	Body       []byte
}

// AssertStatus fails the test immediately when the status differs from want.
func (r *Response) AssertStatus(want int) *Response {
	r.t.Helper()
	if r.StatusCode != want {
		r.t.Fatalf("maniflextest: status got %d, want %d\nbody: %s", r.StatusCode, want, r.Body)
	}
	return r
}

// Decode unmarshals the complete response body into dst.
func (r *Response) Decode(dst any) *Response {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		r.t.Fatalf("maniflextest: decode response: %v\nbody: %s", err, r.Body)
	}
	return r
}

// JSON decodes the response body as a JSON object.
func (r *Response) JSON() map[string]any {
	r.t.Helper()
	var value map[string]any
	r.Decode(&value)
	return value
}

// Data returns the response's data object.
func (r *Response) Data() map[string]any {
	r.t.Helper()
	data, ok := r.JSON()["data"].(map[string]any)
	if !ok {
		r.t.Fatalf("maniflextest: response data is not an object\nbody: %s", r.Body)
	}
	return data
}

// DataList returns the response's data array.
func (r *Response) DataList() []any {
	r.t.Helper()
	data, ok := r.JSON()["data"].([]any)
	if !ok {
		r.t.Fatalf("maniflextest: response data is not an array\nbody: %s", r.Body)
	}
	return data
}

// ErrorCode returns error.code from an error response.
func (r *Response) ErrorCode() string {
	r.t.Helper()
	value, ok := r.JSON()["error"].(map[string]any)
	if !ok {
		r.t.Fatalf("maniflextest: response error is not an object\nbody: %s", r.Body)
	}
	code, ok := value["code"].(string)
	if !ok {
		r.t.Fatalf("maniflextest: response error.code is not a string\nbody: %s", r.Body)
	}
	return code
}

// ID returns data.id from a response.
func (r *Response) ID() string {
	r.t.Helper()
	id, ok := r.Data()["id"].(string)
	if !ok {
		r.t.Fatalf("maniflextest: response data.id is not a string\nbody: %s", r.Body)
	}
	return id
}

// DecodeData decodes a response's data value into T.
func DecodeData[T any](r *Response) T {
	r.t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	r.Decode(&envelope)
	return envelope.Data
}

// DecodeDataList decodes a response's data array into []T.
func DecodeDataList[T any](r *Response) []T {
	r.t.Helper()
	var envelope struct {
		Data []T `json:"data"`
	}
	r.Decode(&envelope)
	return envelope.Data
}

func (s *Server) fatalf(format string, args ...any) {
	s.t.Helper()
	s.t.Fatalf(format, args...)
}
