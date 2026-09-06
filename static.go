package maniflex

import (
	"errors"
	"html"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// wellKnownSegment is the one dot-prefixed path segment the static handler will
// descend into.
//
// It is an IANA-registered URI prefix (RFC 8615), not a name invented here, and
// the things under it are meant to be fetched by strangers: ACME http-01
// challenges, security.txt, apple-app-site-association, assetlinks.json. Refusing
// it along with the rest would stop certificate renewal — a failure that surfaces
// weeks later as an expired certificate rather than as a 404 anyone notices.
const wellKnownSegment = ".well-known"

// staticIndexFile is served for a directory request when it is present.
const staticIndexFile = "index.html"

// staticHandler serves cfg.StaticDir, and is deliberately not http.FileServer.
//
// http.Dir has no opinion about any of the following, and the mount had none
// either (audit HTTP-9):
//
//   - Every method reached it, so DELETE /static/app.js returned the file with
//     200 — chi's Handle registers a route for all of them.
//   - Dotfiles were served, so a StaticDir that is also a working tree published
//     .env and .git/config.
//   - A directory with no index.html rendered a listing of itself.
//   - Symlinks were followed out of the root, so a link anywhere under StaticDir
//     read whatever it pointed at.
//
// The first is fixed at the route; the rest are why this exists. Wrapping
// http.FileServer cannot do it: it needs Open on a directory to succeed so it can
// look for index.html, and a Readdir made to fail renders 500 rather than 404.
//
// Containment is os.Root, the same mechanism LocalStorage uses. It resolves every
// component inside the root, so a symlink cannot leave it and neither can "..",
// on Windows as well as Unix. A symlink with an absolute target is refused even
// when it happens to land inside: os.Root cannot confirm that without resolving
// the path outside its own walk, which is the race it exists to remove. Relative
// links within the tree — the ordinary kind — are followed.
type staticHandler struct {
	dir     string
	listing bool
	logger  *slog.Logger
}

// newStaticHandler prepares to serve dir, checking once that it can be opened as
// a root so a misconfiguration is reported at boot rather than on the first
// request.
//
// The root is reopened per request rather than held. A directory handle kept for
// the life of the process pins that directory: a deploy that swaps the asset
// directory would go on serving the old inode, and on Windows the handle blocks
// the old one from being removed at all. The cost is one openat per request,
// against the several os.Root already does resolving the path a component at a
// time — which is what keeps a symlink inside the root.
func newStaticHandler(dir string, listing bool, l *slog.Logger) (*staticHandler, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if err := root.Close(); err != nil {
		return nil, err
	}
	return &staticHandler{dir: dir, listing: listing, logger: l}, nil
}

func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := staticFilePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	f, info, ok := h.open(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	if info.IsDir() {
		// A directory URL must end in "/" or every relative link inside the
		// document it resolves to would be built against the wrong base.
		//
		// The redirect is relative to the request, because StripPrefix has
		// already removed the mount from r.URL.Path and the absolute target is
		// no longer reachable from here: "admin/" resolves against
		// /static/admin to /static/admin/. name is "." for the mount root
		// itself, which is already the slashed form and must not redirect —
		// StripPrefix leaves that as the empty path, not "/".
		if name != "." && !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, path.Base(name)+"/", http.StatusMovedPermanently)
			return
		}
		h.serveDir(w, r, name, f)
		return
	}

	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// serveDir answers a directory request with its index.html, or with 404 unless
// Config.StaticDirectoryListing is set.
//
// The listing is off by default because it is an information leak whenever it is
// not deliberate: it names every file in the directory, including ones nothing
// links to. The previous behaviour was the reverse — a listing unless an
// index.html happened to be there — so a directory added later, or one whose
// index was renamed, started publishing its own contents with nothing said.
func (h *staticHandler) serveDir(w http.ResponseWriter, r *http.Request, name string, dir *os.File) {
	index := path.Join(name, staticIndexFile)
	if f, info, ok := h.open(index); ok {
		defer f.Close()
		if !info.IsDir() {
			http.ServeContent(w, r, info.Name(), info.ModTime(), f)
			return
		}
	}
	if !h.listing {
		http.NotFound(w, r)
		return
	}
	h.serveListing(w, r, dir)
}

// serveListing renders the directory as links, skipping entries the handler
// would refuse to serve anyway — a listing that advertises a 404 is worse than
// one that does not mention it.
func (h *staticHandler) serveListing(w http.ResponseWriter, r *http.Request, dir *os.File) {
	entries, err := dir.Readdirnames(-1)
	if err != nil {
		h.logger.Error("static: reading directory failed",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Not indexed by search engines and not stored: a listing is a view of the
	// filesystem, and one cached copy outlives whatever it described.
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("<pre>\n"))
	for _, e := range entries {
		if !servableSegment(e) {
			continue
		}
		_, _ = w.Write([]byte("<a href=\"" + escapeStaticHref(e) + "\">" + escapeStaticText(e) + "</a>\n"))
	}
	_, _ = w.Write([]byte("</pre>\n"))
}

// open resolves name inside the root. A path that does not exist, cannot be
// read, or leaves the root is reported the same way — as absent — so the handler
// answers 404 without saying which it was.
func (h *staticHandler) open(name string) (*os.File, fs.FileInfo, bool) {
	root, err := os.OpenRoot(h.dir)
	if err != nil {
		h.logger.Error("static: opening the static directory failed",
			slog.String("dir", h.dir), slog.String("error", err.Error()))
		return nil, nil, false
	}
	defer root.Close()

	f, err := root.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.logger.Debug("static: open refused",
				slog.String("name", name), slog.String("error", err.Error()))
		}
		return nil, nil, false
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, false
	}
	return f, info, true
}

// staticFilePath turns a request path into a name relative to the root, or
// reports that it names nothing servable.
func staticFilePath(urlPath string) (string, bool) {
	cleaned := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	trimmed := strings.Trim(cleaned, "/")
	if trimmed == "" {
		return ".", true
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if !servableSegment(seg) {
			return "", false
		}
	}
	return trimmed, true
}

// servableSegment reports whether one path component may be descended into.
//
// A leading dot is the rule: it covers .env, .git, .htpasswd, .ssh, .DS_Store and
// everything like them in one line, rather than a blocklist that is always one
// filename out of date. .well-known is the documented exception.
func servableSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	if strings.HasPrefix(seg, ".") {
		return seg == wellKnownSegment
	}
	return true
}

// escapeStaticHref and escapeStaticText keep a filename from breaking out of the
// attribute or the element it is written into. Filenames come from a directory
// the operator published, but a build step can put anything in one.
func escapeStaticHref(name string) string {
	return (&url.URL{Path: name}).String()
}

func escapeStaticText(name string) string {
	return html.EscapeString(name)
}
