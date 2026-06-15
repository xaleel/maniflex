package maniflex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// fileHandlers holds standalone file upload/download/delete handlers.
// These operate outside the model pipeline (like the health endpoint).
type fileHandlers struct {
	storage FileStorage
}

func newFileHandlers(storage FileStorage) *fileHandlers {
	return &fileHandlers{storage: storage}
}

// Upload handles POST /files — standalone file upload not tied to a model.
// Accepts multipart/form-data with a single "file" field.
// Returns 201 with {"data": {"key":"...","content_type":"...","size":123,"filename":"..."}}.
func (fh *fileHandlers) Upload() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.storage == nil {
			writeJSONError(w, http.StatusNotImplemented, "NO_STORAGE",
				"file storage not configured")
			return
		}

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSONError(w, http.StatusBadRequest, "MULTIPART_ERROR",
				fmt.Sprintf("failed to parse multipart form: %s", err.Error()))
			return
		}
		defer r.MultipartForm.RemoveAll()

		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "NO_FILE",
				"missing 'file' field in multipart form")
			return
		}
		defer file.Close()

		key := fmt.Sprintf("uploads/%s/%s", uuid.New().String(),
			sanitizeFilename(header.Filename))

		meta := FileMeta{
			Key:         key,
			ContentType: header.Header.Get("Content-Type"),
			Size:        header.Size,
			Filename:    header.Filename,
		}

		if err := fh.storage.Store(r.Context(), key, file, meta); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "STORE_ERROR",
				fmt.Sprintf("failed to store file: %s", err.Error()))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"data": meta,
		})
	}
}

// Serve handles GET /files/* — streams a file from storage to the client.
func (fh *fileHandlers) Serve() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.storage == nil {
			writeJSONError(w, http.StatusNotImplemented, "NO_STORAGE",
				"file storage not configured")
			return
		}

		key := chi.URLParam(r, "*")
		if key == "" {
			writeJSONError(w, http.StatusBadRequest, "MISSING_KEY",
				"file key is required")
			return
		}
		uriDecoded, err := url.QueryUnescape(key)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "DECODE_ERROR",
				"Failed to unescape URI encoded key")
			return
		}

		rc, meta, err := fh.storage.Retrieve(r.Context(), uriDecoded)
		if err != nil {
			if errors.Is(err, ErrFileNotFound) {
				writeJSONError(w, http.StatusNotFound, "FILE_NOT_FOUND",
					fmt.Sprintf("file %q not found", key))
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "RETRIEVE_ERROR",
				fmt.Sprintf("failed to retrieve file: %s", err.Error()))
			return
		}
		defer rc.Close()

		writeFileResponse(w, meta, rc)
	}
}

// writeFileResponse sets content headers from FileMeta and streams the body.
// Shared by /files/* and the per-model attachment route (OpReadAttachment) so
// the two paths emit identical headers.
func writeFileResponse(w http.ResponseWriter, meta FileMeta, rc io.Reader) {
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	if meta.Filename != "" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`inline; filename="%s"`, meta.Filename))
	}
	if meta.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	}
	io.Copy(w, rc)
}

// Delete handles DELETE /files/* — removes a file from storage.
// Returns 204 No Content on success.
func (fh *fileHandlers) Delete() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.storage == nil {
			writeJSONError(w, http.StatusNotImplemented, "NO_STORAGE",
				"file storage not configured")
			return
		}

		key := chi.URLParam(r, "*")
		if key == "" {
			writeJSONError(w, http.StatusBadRequest, "MISSING_KEY",
				"file key is required")
			return
		}

		// Delete directly. The previous Exists+Delete sequence was a TOCTOU race:
		// two concurrent deletes of the same key both passed Exists, then one
		// hit a successful 204 and the other returned a 500 "DELETE_ERROR".
		// Translating ErrFileNotFound here keeps the missing-key 404 contract
		// without the extra round-trip.
		if err := fh.storage.Delete(r.Context(), key); err != nil {
			if errors.Is(err, ErrFileNotFound) {
				writeJSONError(w, http.StatusNotFound, "FILE_NOT_FOUND",
					fmt.Sprintf("file %q not found", key))
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "DELETE_ERROR",
				fmt.Sprintf("failed to delete file: %s", err.Error()))
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// wrapFileMiddleware runs the configured Config.FileMiddleware chain in front
// of a standalone /files handler. Each MiddlewareFunc sees a synthesised
// ServerContext (no Model, no Operation — those concepts don't apply outside
// the model pipeline) that exposes Request, Writer, Ctx, RequestID, and the
// service-wide logger, which is enough surface for auth middleware to read
// the Authorization header and populate ctx.Auth or short-circuit with
// ctx.Response.
//
// When the chain runs through without short-circuiting, the wrapped file
// handler is invoked. When any middleware sets ctx.Response, it is written
// to the wire and the file handler is not called. When a middleware returns
// an error, the response is a generic 500 — middleware that wants a specific
// status should call ctx.Abort instead of returning.
func wrapFileMiddleware(cfg *Config, h http.HandlerFunc) http.Handler {
	if len(cfg.FileMiddleware) == 0 {
		return h
	}
	chain := append([]MiddlewareFunc(nil), cfg.FileMiddleware...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := &ServerContext{
			Request:     r,
			Writer:      w,
			Ctx:         r.Context(),
			RequestID:   r.Header.Get("X-Request-Id"),
			logger:      cfg.logger(),
			serviceName: cfg.ServiceName,
			trace:       cfg.traceConfig(),
		}

		i := 0
		var run func() error
		run = func() error {
			if ctx.Response != nil {
				return nil // a prior middleware short-circuited
			}
			if i >= len(chain) {
				h.ServeHTTP(w, r)
				return nil
			}
			mw := chain[i]
			i++
			return mw(ctx, run)
		}

		if err := run(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "INTERNAL",
				"internal server error")
			return
		}
		if ctx.Response != nil {
			ctx.Response.Write(w)
		}
	})
}

// writeJSONError writes a maniflex-style error envelope.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

// maxFilenameLen caps the post-sanitization filename to keep storage keys
// bounded — the filename is appended to the key, so an unbounded user-supplied
// name produces an unbounded S3 key.
const maxFilenameLen = 120

// sanitizeFilename collapses a user-supplied filename to a safe storage-key
// component. It restricts the charset to `[A-Za-z0-9._-]`, strips leading
// dots to prevent hidden-file confusion, truncates to maxFilenameLen, and
// falls back to "unnamed" for empty / pathological inputs.
//
// Beyond directory traversal (covered by stripping `/` and `\`), the previous
// implementation let control characters through — `\r` / `\n` corrupt the
// `Content-Disposition` header on download, and `..` could survive inside a
// longer name. The charset filter eliminates both classes.
func sanitizeFilename(name string) string {
	// Drop everything that isn't safe ASCII; map anything else to `_`.
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	cleaned := b.String()
	// Strip leading dots so `.htaccess` style names don't become hidden files
	// in the storage layout.
	cleaned = strings.TrimLeft(cleaned, ".")
	if len(cleaned) > maxFilenameLen {
		cleaned = cleaned[:maxFilenameLen]
	}
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		cleaned = "unnamed"
	}
	return cleaned
}
