package admin

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

//go:embed static/*
var embeddedStatic embed.FS

// templateFuncs are available to both embedded and user-overridden templates.
var templateFuncs = template.FuncMap{
	"prettify": prettify,
}

// pageNames are the content templates composed with layout.html.
var pageNames = []string{"dashboard", "list", "detail", "form", "error"}

// templateSet holds one fully-composed template per page.
type templateSet struct {
	pages map[string]*template.Template
}

// loadTemplates composes layout.html with each page template. Every file is
// resolved through the per-template overlay: if override is non-nil and
// contains the file, that copy wins; otherwise the embedded default is used.
func loadTemplates(override fs.FS) (*templateSet, error) {
	read := func(name string) ([]byte, error) {
		if override != nil {
			if b, err := fs.ReadFile(override, name); err == nil {
				return b, nil
			}
		}
		return embeddedTemplates.ReadFile("templates/" + name)
	}

	layout, err := read("layout.html")
	if err != nil {
		return nil, fmt.Errorf("loading layout.html: %w", err)
	}

	ts := &templateSet{pages: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		body, err := read(name + ".html")
		if err != nil {
			return nil, fmt.Errorf("loading %s.html: %w", name, err)
		}
		t := template.New(name).Funcs(templateFuncs)
		// layout.html becomes the executable body; the page file only adds the
		// "content" definition it invokes.
		if _, err := t.Parse(string(layout)); err != nil {
			return nil, fmt.Errorf("parsing layout.html for %s: %w", name, err)
		}
		if _, err := t.Parse(string(body)); err != nil {
			return nil, fmt.Errorf("parsing %s.html: %w", name, err)
		}
		ts.pages[name] = t
	}
	return ts, nil
}

// render writes a composed page to w.
func (a *admin) render(w http.ResponseWriter, page string, data viewData) {
	t, ok := a.tmpl.pages[page]
	if !ok {
		a.cfg.logger().Error("admin template is missing", "page", page)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setSecurityHeaders(w)
	if err := t.Execute(w, data); err != nil {
		// The status line and some body bytes may already be flushed, so the
		// failure can only be logged; appending it would expose template paths
		// and data-shape details in an otherwise successful response.
		a.cfg.logger().Error("admin template render failed", "page", page, "error", err)
	}
}

// setSecurityHeaders hardens every panel response (audit ADM-4).
//
// Panel pages carry record data and the CSRF token, so no-store keeps them out
// of shared caches and off the back button, and DENY stops the delete form being
// framed. No Content-Security-Policy is emitted: the shipped templates use two
// inline handlers, and Config.StaticFS *replaces* the embedded bundle rather
// than overlaying it, so a policy strict enough to be worth having would break
// any panel with a custom static bundle or custom templates.
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
}

// renderError writes the error page with the given HTTP status.
func (a *admin) renderError(w http.ResponseWriter, status int, msg string) {
	if status >= http.StatusInternalServerError && status <= 599 {
		a.cfg.logger().Error("admin server error response",
			"status", status,
			"error", msg,
		)
		if text := http.StatusText(status); text != "" {
			msg = strings.ToLower(text)
		} else {
			msg = "server error"
		}
	}
	t, ok := a.tmpl.pages["error"]
	if !ok {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	setSecurityHeaders(w)
	w.WriteHeader(status)
	_ = t.Execute(w, viewData{
		Title:  a.cfg.Title,
		Prefix: a.cfg.PathPrefix,
		Nav:    a.nav(),
		Error:  &errorData{Status: status, Message: msg},
	})
}
