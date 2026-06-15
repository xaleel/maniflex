package admin

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
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
		http.Error(w, "admin: unknown page "+page, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		// The status line and some body bytes may already be flushed, so this
		// can only append a diagnostic comment.
		fmt.Fprintf(w, "\n<!-- admin render error: %v -->", err)
	}
}

// renderError writes the error page with the given HTTP status.
func (a *admin) renderError(w http.ResponseWriter, status int, msg string) {
	t, ok := a.tmpl.pages["error"]
	if !ok {
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = t.Execute(w, viewData{
		Title:  a.cfg.Title,
		Prefix: a.cfg.PathPrefix,
		Nav:    a.nav(),
		Error:  &errorData{Status: status, Message: msg},
	})
}
