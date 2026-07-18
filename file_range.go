package maniflex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// byteRange is a resolved, absolute window into a stored object: the bytes
// [start, start+length) of an object that is total bytes long. Resolving a
// Range header into one of these is what turns the relative forms the header
// allows ("bytes=-500", "bytes=1024-") into something a backend can fetch.
type byteRange struct {
	start  int64
	length int64
	total  int64
}

// contentRange renders the window as a Content-Range header value.
func (br byteRange) contentRange() string {
	return fmt.Sprintf("bytes %d-%d/%d", br.start, br.start+br.length-1, br.total)
}

// errRangeUnsatisfiable marks a Range that is well-formed but asks for bytes
// the object does not have — the one case that must answer 416 rather than
// quietly serving the whole thing.
var errRangeUnsatisfiable = errors.New("range not satisfiable")

// parseByteRange resolves a Range header against a known object size.
//
// It returns ok=false for "no usable range — serve the whole object with 200",
// which RFC 9110 §14.2 permits for any Range a server chooses not to honour.
// That covers a missing or malformed header, an object of unknown size, and
// multi-range requests: those would need a multipart/byteranges body, and a
// client that asks for several windows at once is served the whole object and
// can slice it itself.
//
// Only errRangeUnsatisfiable means 416.
func parseByteRange(hdr string, size int64) (byteRange, bool, error) {
	spec, found := strings.CutPrefix(strings.TrimSpace(hdr), "bytes=")
	if !found || size <= 0 {
		return byteRange{}, false, nil
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.ContainsRune(spec, ',') {
		return byteRange{}, false, nil
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return byteRange{}, false, nil
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		return suffixByteRange(last, size)
	}
	return offsetByteRange(first, last, size)
}

// suffixByteRange resolves "bytes=-N" — the last N bytes of the object. A
// suffix longer than the object is clamped to the whole object rather than
// refused, as the spec requires.
func suffixByteRange(last string, size int64) (byteRange, bool, error) {
	n, err := strconv.ParseInt(last, 10, 64)
	if err != nil || n < 0 {
		return byteRange{}, false, nil
	}
	if n == 0 {
		// "the last zero bytes" — well-formed, but nothing can satisfy it.
		return byteRange{}, false, errRangeUnsatisfiable
	}
	if n > size {
		n = size
	}
	return byteRange{start: size - n, length: n, total: size}, true, nil
}

// offsetByteRange resolves "bytes=S-" and "bytes=S-E". An end past the last
// byte is clamped; a start past it is unsatisfiable.
func offsetByteRange(first, last string, size int64) (byteRange, bool, error) {
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return byteRange{}, false, nil
	}
	if start >= size {
		return byteRange{}, false, errRangeUnsatisfiable
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return byteRange{}, false, nil
		}
		if end > size-1 {
			end = size - 1
		}
	}
	return byteRange{start: start, length: end - start + 1, total: size}, true, nil
}

// fileServeError is a storage-read failure, returned rather than written so
// each caller can map it onto its own error convention — writeJSONError for
// GET /files/*, ctx.Abort for the per-model attachment route.
type fileServeError struct {
	Status  int
	Code    string
	Message string
}

// serveStoredFile writes the object at key to w, honouring a Range request
// when the backend or the reader can satisfy one. It is the single download
// path behind both GET /files/* and the per-model attachment route, so the two
// emit identical headers and identical range semantics.
//
// Range support resolves in three tiers:
//
//  1. The backend implements RangeRetriever — the window is fetched from
//     storage directly, so on a remote backend the bytes outside it never
//     cross the wire (or the egress bill). One Stat sizes the object first,
//     which is what makes "bytes=-500" resolvable and an out-of-range request
//     a 416 rather than a 200.
//  2. It does not, but the reader it hands back is seekable — net/http's
//     ServeContent takes over, bringing If-Range and multi-range with it.
//  3. Neither — the Range header is ignored and the whole object is served
//     with 200, which the spec explicitly allows.
func serveStoredFile(ctx context.Context, w http.ResponseWriter, r *http.Request, store FileStorage, key string) *fileServeError {
	rangeHdr := ""
	if r != nil {
		rangeHdr = r.Header.Get("Range")
	}

	rr, rangeCapable := store.(RangeRetriever)
	if rangeCapable {
		// Honest advertisement: only claim ranges we can actually serve from
		// storage. The seekable path below sets it for itself.
		w.Header().Set("Accept-Ranges", "bytes")
		if rangeHdr != "" {
			return serveBackendRange(ctx, w, store, rr, key, rangeHdr)
		}
	}

	rc, meta, err := store.Retrieve(ctx, key)
	if err != nil {
		return retrieveFileError(key, err)
	}
	defer rc.Close()

	if rs, ok := rc.(io.ReadSeeker); ok && rangeHdr != "" {
		setFileHeaders(w, meta)
		w.Header().Set("Accept-Ranges", "bytes")
		// A zero modtime tells ServeContent to skip the conditional-request
		// machinery it cannot answer — FileMeta carries no modification time.
		http.ServeContent(w, r, meta.Filename, time.Time{}, rs)
		return nil
	}

	writeFileResponse(w, meta, rc)
	return nil
}

// serveBackendRange fetches just the requested window from a RangeRetriever
// backend and writes the 206.
func serveBackendRange(ctx context.Context, w http.ResponseWriter, store FileStorage, rr RangeRetriever, key, hdr string) *fileServeError {
	// The object's size is what the header is relative to, so it has to be
	// known before the range means anything. Stat is the cheap way to learn it
	// (a HEAD on S3) — the whole point of this path is not fetching the body.
	stat, err := store.Stat(ctx, key)
	if err != nil {
		return retrieveFileError(key, err)
	}

	br, ok, rangeErr := parseByteRange(hdr, stat.Size)
	if errors.Is(rangeErr, errRangeUnsatisfiable) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", stat.Size))
		writeJSONError(w, http.StatusRequestedRangeNotSatisfiable, "RANGE_NOT_SATISFIABLE",
			fmt.Sprintf("requested range is outside the %d-byte object", stat.Size))
		return nil
	}
	if !ok {
		return serveWholeObject(ctx, w, store, key)
	}

	rc, meta, err := rr.RetrieveRange(ctx, key, br.start, br.length)
	if err != nil {
		return retrieveFileError(key, err)
	}
	defer rc.Close()

	// A window's own metadata describes the window — its length is not the
	// object's. Content type and filename come from the Stat that sized it.
	if meta.ContentType == "" {
		meta.ContentType = stat.ContentType
	}
	if meta.Filename == "" {
		meta.Filename = stat.Filename
	}

	setFileHeaders(w, meta)
	w.Header().Set("Content-Range", br.contentRange())
	w.Header().Set("Content-Length", strconv.FormatInt(br.length, 10))
	w.WriteHeader(http.StatusPartialContent)
	io.Copy(w, rc)
	return nil
}

// serveWholeObject is the 200 fallback taken when a range-capable backend is
// handed a Range it cannot use.
func serveWholeObject(ctx context.Context, w http.ResponseWriter, store FileStorage, key string) *fileServeError {
	rc, meta, err := store.Retrieve(ctx, key)
	if err != nil {
		return retrieveFileError(key, err)
	}
	defer rc.Close()
	writeFileResponse(w, meta, rc)
	return nil
}

// retrieveFileError maps a storage error onto the response both download
// routes give for it.
func retrieveFileError(key string, err error) *fileServeError {
	if errors.Is(err, ErrFileNotFound) {
		return &fileServeError{
			Status:  http.StatusNotFound,
			Code:    "FILE_NOT_FOUND",
			Message: fmt.Sprintf("file %q not found", key),
		}
	}
	return &fileServeError{
		Status:  http.StatusInternalServerError,
		Code:    "RETRIEVE_ERROR",
		Message: fmt.Sprintf("failed to retrieve file: %s", err.Error()),
	}
}
