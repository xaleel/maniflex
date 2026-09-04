package maniflex

import (
	"fmt"
	"net/http"
	"strings"
)

// builtinRouteShapes returns the routes the framework mounts under PathPrefix
// itself, in the same routeShape form modelRouteShapes uses for models.
//
// Only what this configuration actually serves is listed. A disabled probe, an
// unpublished spec document, a search endpoint nobody enabled — none of them are
// mounted, so none of them are reserved, and taking their path is a legitimate
// thing to do. Config.Probes.Health.Disabled with a hand-written /health is the
// case that matters: it works today and must keep working.
//
// The shapes are relative to PathPrefix, because that is what an action's Path
// and a model's table name are measured against — both are mounted inside
// buildRouter's r.Route(cfg.PathPrefix, …).
func builtinRouteShapes(cfg *Config, p *Pipeline, globalSearch *GlobalSearchConfig, asyncCfg *AsyncAPIConfig) []routeShape {
	var shapes []routeShape

	// Probes. mountProbes registers GET only, so a POST at the same path is not
	// a collision — it addresses a route the framework does not serve.
	for _, probe := range []struct {
		cfg  ProbeConfig
		path string
	}{
		{cfg.Probes.Live, "live"},
		{cfg.Probes.Ready, "ready"},
		{cfg.Probes.Health, "health"},
	} {
		if probe.cfg.Disabled {
			continue
		}
		shapes = append(shapes, routeShape{
			kind:     "probe",
			segments: []string{probe.path},
			methods:  []string{http.MethodGet},
		})
	}

	// Generated documentation, under the same conditions buildRouter mounts it.
	docsConfigured := cfg.Documentation.Public || len(cfg.Documentation.Middleware) > 0
	if docsConfigured || p.OpenAPI.Auth.configured() {
		shapes = append(shapes, routeShape{
			kind:     "OpenAPI document",
			segments: []string{"openapi.json"},
			methods:  []string{http.MethodGet},
		})
	}
	if docsConfigured && asyncCfg != nil {
		shapes = append(shapes, routeShape{
			kind:     "AsyncAPI document",
			segments: []string{"asyncapi.json"},
			methods:  []string{http.MethodGet},
		})
	}

	if globalSearch != nil {
		shapes = append(shapes, routeShape{
			kind:     "global search",
			segments: pathSegments(globalSearch.Path),
			methods:  []string{http.MethodGet},
		})
	}

	if cfg.FilesConfig.MountEndpoints {
		shapes = append(shapes,
			routeShape{kind: "file upload", segments: []string{"files"}, methods: []string{http.MethodPost}},
			routeShape{
				kind:     "file download/delete",
				segments: []string{"files", "*"},
				methods:  []string{http.MethodGet, http.MethodDelete},
			},
		)
	}

	return shapes
}

// shapesCollide reports whether two routes resolve any of the same requests:
// the same path structure and at least one method in common.
func shapesCollide(a, b routeShape) bool {
	for _, method := range a.methods {
		if actionShadowsRoute(method, a.segments, b) {
			return true
		}
	}
	return false
}

// collectReservedPathIssues refuses a registration that would silently take over
// a route the framework serves.
//
// chi's InsertRoute overwrites rather than conflicting, and the framework's own
// routes are mounted before any model or action, so the later registration wins
// with nothing said anywhere: an action at /live answers the liveness probe, and
// a model whose table is "health" retires the health endpoint. An orchestrator
// goes on polling a path that now runs application code.
//
// It runs here rather than in checkActionConflict because that fires at
// Action() time, when the picture is incomplete: EnableGlobalSearch may not have
// been called yet, and a model registered afterwards is not seen at all.
func collectReservedPathIssues(
	cfg *Config,
	reg *Registry,
	p *Pipeline,
	actions []ActionConfig,
	globalSearch *GlobalSearchConfig,
	asyncCfg *AsyncAPIConfig,
	issues *issueList,
) {
	builtins := builtinRouteShapes(cfg, p, globalSearch, asyncCfg)
	if len(builtins) == 0 {
		return
	}

	for _, action := range actions {
		method := strings.ToUpper(action.Method)
		segs := pathSegments(action.Path)
		for _, builtin := range builtins {
			if actionShadowsRoute(method, segs, builtin) {
				issues.add("route", "action %s %s would replace the framework's %s route at %s — "+
					"chi overwrites rather than collides, so the built-in endpoint would stop "+
					"answering with nothing reported%s",
					method, action.Path, builtin.kind, joinPath(cfg.PathPrefix, builtin.segments),
					freeingHint(builtin))
			}
		}
	}

	for _, meta := range reg.All() {
		if meta.Config.Headless {
			continue
		}
		for _, shape := range modelRouteShapes(meta) {
			for _, builtin := range builtins {
				if shapesCollide(shape, builtin) {
					issues.add("route", "model %q mounts its %s route at %s, which is the "+
						"framework's %s route — the built-in endpoint would stop answering "+
						"with nothing reported%s",
						meta.Name, shape.kind, joinPath(cfg.PathPrefix, shape.segments),
						builtin.kind, freeingHint(builtin))
				}
			}
		}
	}
}

// freeingHint names the switch that makes a reserved path available, for the
// built-ins that have one. Refusing a registration without saying how to get the
// path is the unhelpful half of this check.
func freeingHint(builtin routeShape) string {
	if builtin.kind != "probe" || len(builtin.segments) != 1 {
		return ""
	}
	return fmt.Sprintf(" (set Config.Probes.%s.Disabled to serve this path yourself)",
		strings.ToUpper(builtin.segments[0][:1])+builtin.segments[0][1:])
}

// joinPath renders an absolute route path for an error message.
func joinPath(prefix string, segments []string) string {
	return "/" + strings.Trim(strings.Trim(prefix, "/")+"/"+strings.Join(segments, "/"), "/")
}

// collectStaticShadowIssues refuses a static mount that would swallow the API.
//
// mountStatic registers <prefix>/* at the router root, and chi's InsertRoute
// overwrites, so a static prefix equal to PathPrefix takes /api/* from the
// mounted API sub-router: every generated route, every action and every probe
// answers 404 from the file server, and nothing says so. A prefix nested inside
// PathPrefix or sitting at the root is fine — the API's own mount is the more
// specific route and keeps winning — so only the exact overlap is refused.
func collectStaticShadowIssues(cfg *Config, issues *issueList) {
	if cfg.StaticDisabled || cfg.StaticDir == "" {
		return
	}
	static := strings.TrimSuffix(staticPrefix(cfg), "/")
	prefix := strings.TrimSuffix(cfg.PathPrefix, "/")
	if static != prefix {
		return
	}
	issues.add("static", "Config.StaticPrefix %q is Config.PathPrefix, so the static file "+
		"server would take over %s/* and every API route under it would answer 404",
		staticPrefix(cfg), prefix)
}
