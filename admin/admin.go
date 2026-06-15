// Package admin provides an opt-in, server-rendered administration panel for
// a maniflex server.
//
// It is a satellite module: the core maniflex module has no awareness of it, and
// importing it is the only way it has any effect. The panel introspects the
// server's model registry to render its views, and reads/writes data by
// issuing in-process HTTP requests against the server's own generated REST
// API — so every request still flows through the full auth/validate/pipeline
// stack. The admin never bypasses the pipeline or touches the DB directly.
//
// Usage:
//
//	server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
//	server.MustRegister(User{}, Post{})
//	server.SetDB(db)
//
//	adminHandler := admin.Mount(server, admin.Config{
//	    Title:                "My Admin",
//	    AllowUnauthenticated: true, // local dev only
//	})
//
//	r := chi.NewRouter()
//	maniflex.Mount(r, server)
//	r.Mount("/admin", http.StripPrefix("/admin", adminHandler))
//	http.ListenAndServe(":8080", r)
package admin

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/xaleel/maniflex"
)

// Config controls how the admin panel is mounted and rendered.
type Config struct {
	// PathPrefix is where the panel is mounted, used for routing and link
	// generation. Default: "/admin". The returned handler matches paths under
	// this prefix, so mount it at the same prefix on your router.
	PathPrefix string

	// Title is shown in the panel header. Default: "maniflex admin".
	Title string

	// Templates, when non-nil, is consulted before the embedded defaults for
	// every template file. Drop in e.g. "list.html" to override just that
	// view; any file the override FS lacks falls through to the embedded
	// default. See the templates/ directory for the file names.
	Templates fs.FS

	// StaticFS, when non-nil, replaces the embedded static asset bundle
	// served under PathPrefix/static/ (e.g. a custom admin.css).
	StaticFS fs.FS

	// Models optionally whitelists which registered models appear in the
	// panel, by Go struct name. Empty means every registered model.
	Models []string

	// ReadOnly, when true, serves only the dashboard, list, and detail views:
	// no create/edit/delete routes are mounted and their controls are hidden.
	ReadOnly bool

	// Auth wraps the panel handler with an authentication gate. Required
	// unless AllowUnauthenticated is set.
	Auth func(http.Handler) http.Handler

	// AllowUnauthenticated permits mounting the panel with no Auth gate.
	// Intended for local development only — never set this in production.
	AllowUnauthenticated bool
}

// Mount builds the admin panel handler for server. The returned handler serves
// every panel route under Config.PathPrefix.
//
// Mount must be called after all models are registered and the DB adapter is
// set (it captures server.Handler()), and before server.Start().
//
// It panics if neither Config.Auth nor Config.AllowUnauthenticated is set, so
// an unprotected panel is never shipped by accident.
func Mount(server *maniflex.Server, cfg Config) http.Handler {
	if server == nil {
		panic("admin: Mount requires a non-nil *maniflex.Server")
	}
	if cfg.PathPrefix == "" {
		cfg.PathPrefix = "/admin"
	}
	cfg.PathPrefix = "/" + strings.Trim(cfg.PathPrefix, "/")
	if cfg.Title == "" {
		cfg.Title = "maniflex admin"
	}
	if cfg.Auth == nil && !cfg.AllowUnauthenticated {
		panic("admin: Mount requires Config.Auth, or Config.AllowUnauthenticated=true for local dev")
	}

	a, err := newAdmin(server, cfg)
	if err != nil {
		panic("admin: " + err.Error())
	}

	var h http.Handler = a.routes()
	if cfg.Auth != nil {
		h = cfg.Auth(h)
	}
	return h
}
