package maniflex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// fileHandlers holds standalone file upload/download/delete handlers.
// These operate outside the model pipeline (like the health endpoint).
type fileHandlers struct {
	config FilesConfig
}

func DefaultKeyGen(_ *ServerContext, header *multipart.FileHeader) string {
	return fmt.Sprintf("uploads/%s/%s", uuid.New().String(),
		sanitizeFilename(header.Filename))
}

func newFileHandlers(config FilesConfig) *fileHandlers {
	if config.KeyGen == nil {
		config.KeyGen = DefaultKeyGen
	}
	return &fileHandlers{config}
}

// Upload handles POST /files — standalone file upload not tied to a model.
// Accepts multipart/form-data with a single "file" field.
// Returns 201 with {"data": {"key":"...","content_type":"...","size":123,"filename":"..."}}.
func (fh *fileHandlers) Upload(ctx *ServerContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.config.Storage == nil {
			writeJSONError(w, http.StatusNotImplemented, "NO_STORAGE",
				"file storage not configured")
			return
		}

		// Bound the body before parsing: ParseMultipartForm's argument caps only the
		// in-memory buffer, and the overflow spools to temp files (BUG-5).
		limit := fh.config.uploadLimit()
		r.Body = http.MaxBytesReader(w, r.Body, limit)

		if err := r.ParseMultipartForm(fh.config.uploadMemory()); err != nil {
			if isBodyTooLarge(err) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
					fmt.Sprintf("request body exceeds %s limit", formatByteSize(limit)))
				return
			}
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

		key := fh.config.KeyGen(ctx, header)

		contentType, err := resolveContentType(header.Header.Get("Content-Type"), file)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "FILE_READ_ERROR",
				fmt.Sprintf("failed to read uploaded file: %s", err.Error()))
			return
		}

		meta := FileMeta{
			Key:         key,
			ContentType: contentType,
			Size:        header.Size,
			Filename:    header.Filename,
		}

		if err := fh.config.Storage.Store(r.Context(), key, file, meta); err != nil {
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
func (fh *fileHandlers) Serve(ctx *ServerContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.config.Storage == nil {
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

		rc, meta, err := fh.config.Storage.Retrieve(r.Context(), uriDecoded)
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

// inlineSafeContentTypes lists the media types that are safe to render inline in
// a browser (together with X-Content-Type-Options: nosniff). Anything not on the
// list — notably text/html, image/svg+xml (SVG can carry inline <script>), and
// XML — is served as an attachment so a stored file cannot run as script on the
// API origin (stored XSS).
var inlineSafeContentTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"image/bmp":       true,
	"image/x-icon":    true,
	"application/pdf": true,
	"text/plain":      true,
}

// inlineSafeContentType reports whether ct (a possibly parameterised media type
// such as "text/plain; charset=utf-8") is on the inline allowlist.
func inlineSafeContentType(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return inlineSafeContentTypes[strings.ToLower(strings.TrimSpace(ct))]
}

// writeFileResponse sets content headers from FileMeta and streams the body.
// Shared by /files/* and the per-model attachment route (OpReadAttachment) so
// the two paths emit identical headers.
//
// Security (SEC-4): the browser is told never to MIME-sniff the stored bytes
// (X-Content-Type-Options: nosniff), and only known-safe content types are
// served inline — everything else is forced to download (Content-Disposition:
// attachment) so a stored HTML/SVG file cannot execute script on the API origin.
func writeFileResponse(w http.ResponseWriter, meta FileMeta, rc io.Reader) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if meta.ContentType != "" {
		w.Header().Set("Content-Type", meta.ContentType)
	}
	disposition := "attachment"
	if inlineSafeContentType(meta.ContentType) {
		disposition = "inline"
	}
	if meta.Filename != "" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`%s; filename="%s"`, disposition, meta.Filename))
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}
	if meta.Size > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", meta.Size))
	}
	io.Copy(w, rc)
}

// Delete handles DELETE /files/* — removes a file from storage.
// Returns 204 No Content on success.
func (fh *fileHandlers) Delete(ctx *ServerContext) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fh.config.Storage == nil {
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
		if err := fh.config.Storage.Delete(r.Context(), key); err != nil {
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
func wrapFileMiddleware(cfg *Config, hf func(ctx *ServerContext) http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The file handlers stream directly to the wire (Serve does io.Copy of
		// arbitrarily large files), so the response is committed the moment the
		// handler runs. obs records the outcome without buffering the body,
		// letting AfterMiddlewares observe the result and guarding against a
		// double-write when ctx.Response is set after the body is already sent.
		obs := &responseObserver{ResponseWriter: w}
		ctx := &ServerContext{
			Request:     r,
			Writer:      obs,
			Ctx:         r.Context(),
			RequestID:   r.Header.Get("X-Request-Id"),
			logger:      cfg.logger(),
			serviceName: cfg.ServiceName,
			trace:       cfg.traceConfig(),
		}
		if len(cfg.FilesConfig.BeforeMiddlewares)+len(cfg.FilesConfig.AfterMiddlewares) == 0 {
			hf(ctx).ServeHTTP(obs, r)
			return
		}

		ib := 0
		ia := 0
		var run func() error
		run = func() error {
			if ctx.Response != nil {
				return nil // a prior before-middleware short-circuited
			}
			if ib >= len(cfg.FilesConfig.BeforeMiddlewares) {
				// run handler once, after all BeforeMiddlewares have passed
				if ia == 0 {
					hf(ctx).ServeHTTP(obs, r) // streams straight to the client
				}
				// all AfterMiddlewares ran
				if ia >= len(cfg.FilesConfig.AfterMiddlewares) {
					return nil
				}
				// run AfterMiddlewares
				mw := cfg.FilesConfig.AfterMiddlewares[ia]
				ia++
				return mw(ctx, run)
			}
			mw := cfg.FilesConfig.BeforeMiddlewares[ib]
			ib++
			return mw(ctx, run)
		}

		if err := run(); err != nil {
			// Don't stack a 500 on top of an already-streamed body.
			if !obs.wrote {
				writeJSONError(obs, http.StatusInternalServerError, "INTERNAL",
					"internal server error")
			}
			return
		}

		// The handler streams its response directly. Only fall back to writing
		// ctx.Response when the handler never ran — i.e. a before-middleware
		// short-circuited. An after-middleware that sets ctx.Response once the
		// body is already on the wire cannot rewrite it; warn instead of
		// emitting a corrupt double-write.
		if ctx.Response != nil {
			if obs.wrote {
				cfg.logger().Warn("file middleware: ctx.Response set after the response was already sent; ignoring",
					"status", obs.status)
			} else {
				ctx.Response.Write(obs)
			}
		}
	})
}

// responseObserver wraps the ResponseWriter to record the handler's outcome
// (status code + whether anything was written) WITHOUT buffering the body, so
// file downloads still stream straight to the client. AfterMiddlewares read
// this via ctx.Writer to observe the result; because the body is already on
// the wire, they can run side effects (audit, metrics, cleanup) but cannot
// rewrite a response the handler has already committed.
type responseObserver struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (o *responseObserver) WriteHeader(code int) {
	if !o.wrote {
		o.status, o.wrote = code, true
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(b []byte) (int, error) {
	if !o.wrote {
		o.status, o.wrote = http.StatusOK, true
	}
	return o.ResponseWriter.Write(b)
}

// Status reports the status code the handler sent (0 if nothing was written).
// After-middlewares can read it with ctx.Writer.(interface{ Status() int }).
func (o *responseObserver) Status() int { return o.status }

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
