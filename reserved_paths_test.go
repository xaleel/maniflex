package maniflex

// Audit HTTP-5 — the framework mounts probes, the spec documents, global search
// and the /files endpoints under PathPrefix, and nothing reserved those paths. A
// model or action registered at one of them silently took the route: chi's
// InsertRoute overwrites rather than conflicting, and the built-ins are mounted
// first, so the later registration wins with nothing reported. An orchestrator
// then polls a path that runs application code. config.md said these were
// reserved and that a registration there "collides"; neither was true.

import (
	"net/http"
	"strings"
	"testing"
)

type reservedPathModel struct {
	BaseModel
	Title string `json:"title"`
}

// buildErr assembles the server and returns the startup error, if any.
func buildErr(t *testing.T, build func(*Server)) error {
	t.Helper()
	srv := New(Config{})
	build(srv)
	_, err := srv.handler()
	return err
}

func TestReservedPaths_ActionAtAProbePathIsRefused(t *testing.T) {
	err := buildErr(t, func(srv *Server) {
		srv.MustRegister(reservedPathModel{})
		srv.Action(ActionConfig{
			Method:  http.MethodGet,
			Path:    "/live",
			Handler: func(ctx *ServerContext) error { return nil },
		})
	})
	if err == nil {
		t.Fatal("an action at /live was accepted; it silently replaces the liveness probe")
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Errorf("error does not name the route it would replace: %v", err)
	}
	// The message has to say how to claim the path, or the check is a dead end.
	if !strings.Contains(err.Error(), "Probes.Live.Disabled") {
		t.Errorf("error does not name the switch that frees the path: %v", err)
	}
}

// A method the framework does not serve at that path is not a collision — the
// probes mount GET only, so POST /live addresses a route that does not exist.
func TestReservedPaths_OtherMethodAtAProbePathIsAllowed(t *testing.T) {
	err := buildErr(t, func(srv *Server) {
		srv.MustRegister(reservedPathModel{})
		srv.Action(ActionConfig{
			Method:  http.MethodPost,
			Path:    "/live",
			Handler: func(ctx *ServerContext) error { return nil },
		})
	})
	if err != nil {
		t.Fatalf("POST /live was refused although the probe only mounts GET: %v", err)
	}
}

// Disabling a probe frees its path. This works today and is the documented way
// to serve your own /health, so the reservation must not take it away.
func TestReservedPaths_DisabledProbeFreesItsPath(t *testing.T) {
	srv := New(Config{Probes: ProbesConfig{Health: ProbeConfig{Disabled: true}}})
	srv.MustRegister(reservedPathModel{})
	srv.Action(ActionConfig{
		Method:  http.MethodGet,
		Path:    "/health",
		Handler: func(ctx *ServerContext) error { return nil },
	})
	if _, err := srv.handler(); err != nil {
		t.Fatalf("an action at /health was refused although the probe is disabled: %v", err)
	}
}

func TestReservedPaths_ModelAtAProbePathIsRefused(t *testing.T) {
	err := buildErr(t, func(srv *Server) {
		srv.MustRegister(reservedPathModel{}, ModelConfig{TableName: "live"})
	})
	if err == nil {
		t.Fatal("a model whose table is \"live\" was accepted; its list route replaces the probe")
	}
	if !strings.Contains(err.Error(), "reservedPathModel") || !strings.Contains(err.Error(), "probe") {
		t.Errorf("error names neither the model nor the route: %v", err)
	}
}

// A headless model mounts nothing, so it cannot take a path.
func TestReservedPaths_HeadlessModelAtAProbePathIsAllowed(t *testing.T) {
	err := buildErr(t, func(srv *Server) {
		srv.MustRegister(reservedPathModel{}, ModelConfig{TableName: "live", Headless: true})
	})
	if err != nil {
		t.Fatalf("a headless model was refused although it mounts no routes: %v", err)
	}
}

// The spec documents are only mounted when documentation is configured, so the
// reservation follows that switch rather than the path spelling.
func TestReservedPaths_OpenAPIPathFollowsWhetherItIsMounted(t *testing.T) {
	action := func(srv *Server) {
		srv.MustRegister(reservedPathModel{})
		srv.Action(ActionConfig{
			Method:  http.MethodGet,
			Path:    "/openapi.json",
			Handler: func(ctx *ServerContext) error { return nil },
		})
	}

	// Not published: nothing is mounted there, so the path is free.
	if err := buildErr(t, action); err != nil {
		t.Fatalf("/openapi.json was reserved although documentation is not configured: %v", err)
	}

	// Published: the action would replace it.
	srv := New(Config{Documentation: DocumentationConfig{Public: true}})
	action(srv)
	if _, err := srv.handler(); err == nil {
		t.Fatal("an action at /openapi.json was accepted while the document is published")
	}
}

// Global search is enabled through a Server method, which is why this check
// cannot live in checkActionConflict: at Action() time the endpoint may not
// exist yet.
func TestReservedPaths_GlobalSearchEnabledAfterTheAction(t *testing.T) {
	srv := New(Config{})
	srv.MustRegister(reservedPathModel{})
	srv.Action(ActionConfig{
		Method:  http.MethodGet,
		Path:    "/search",
		Handler: func(ctx *ServerContext) error { return nil },
	})
	srv.EnableGlobalSearch()

	if _, err := srv.handler(); err == nil {
		t.Fatal("an action at /search was accepted although global search was enabled after it")
	}
}

func TestReservedPaths_FileEndpointsAreReservedWhenMounted(t *testing.T) {
	upload := func(srv *Server) {
		srv.MustRegister(reservedPathModel{})
		srv.Action(ActionConfig{
			Method:  http.MethodPost,
			Path:    "/files",
			Handler: func(ctx *ServerContext) error { return nil },
		})
	}

	if err := buildErr(t, upload); err != nil {
		t.Fatalf("/files was reserved although the endpoints are not mounted: %v", err)
	}

	srv := New(Config{FilesConfig: FilesConfig{MountEndpoints: true, AllowPublic: true}})
	upload(srv)
	if _, err := srv.handler(); err == nil {
		t.Fatal("an action at POST /files was accepted while the file endpoints are mounted")
	}
}

// A static prefix equal to PathPrefix takes /api/* from the API's own mount, so
// every generated route, action and probe answers 404 from the file server.
func TestReservedPaths_StaticPrefixEqualToPathPrefixIsRefused(t *testing.T) {
	srv := New(Config{StaticDir: ".", StaticPrefix: "/api"})
	srv.MustRegister(reservedPathModel{})
	_, err := srv.handler()
	if err == nil {
		t.Fatal("StaticPrefix == PathPrefix was accepted; it 404s the whole API")
	}
	if !strings.Contains(err.Error(), "StaticPrefix") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// Nested and disjoint static prefixes are fine: the API's mount is the more
// specific route and keeps winning. Refusing them would be a false positive.
func TestReservedPaths_OtherStaticPrefixesAreAllowed(t *testing.T) {
	for _, prefix := range []string{"/static", "/api/static", "/"} {
		srv := New(Config{StaticDir: ".", StaticPrefix: prefix})
		srv.MustRegister(reservedPathModel{})
		if _, err := srv.handler(); err != nil {
			t.Errorf("StaticPrefix %q was refused: %v", prefix, err)
		}
	}
}

// The default configuration must not trip any of this.
func TestReservedPaths_DefaultServerIsClean(t *testing.T) {
	srv := New(Config{})
	srv.MustRegister(reservedPathModel{})
	if _, err := srv.handler(); err != nil {
		t.Fatalf("an ordinary server was refused: %v", err)
	}
}
