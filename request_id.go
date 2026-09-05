package maniflex

import (
	"net/http"

	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// MaxRequestIDLength bounds an X-Request-Id the framework will adopt from a
// client. Every format in real use is well under it: a UUID is 36 characters,
// chi's own generated id around 33, an AWS X-Ray trace header about 35.
const MaxRequestIDLength = 128

// sanitizeRequestID drops an X-Request-Id the framework is not prepared to carry,
// leaving chi's RequestID middleware to generate one in its place.
//
// chi takes the header verbatim, and that value travels: it is echoed in the
// response, added to every log line the request produces, and persisted — into
// audit records and into the request_id column of a versioned model's history
// table. Unbounded, it is a client-chosen 60 KB string in each of those places,
// once per request, with only Config-level header limits standing in the way.
//
// Nothing here is about injection; Go neutralises CRLF in a header value on the
// way out and slog escapes the value in both its handlers. It is about size, and
// about what an operator reading a log line or a stored audit row should be able
// to assume it looks like.
//
// A rejected header is replaced rather than refused. The id is a convenience,
// and failing a request over a malformed one would trade a cosmetic problem for
// an outage; the client sees which id was actually used in the response header.
//
// This runs ahead of chi's middleware rather than replacing it, so the id key,
// the generator and GetReqID stay chi's — a fork of those to add one check would
// be the larger change.
func sanitizeRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get(chiMiddleware.RequestIDHeader); id != "" && !validRequestID(id) {
			r.Header.Del(chiMiddleware.RequestIDHeader)
		}
		next.ServeHTTP(w, r)
	})
}

// validRequestID reports whether an id may be adopted as sent.
//
// The set is alphanumerics with - _ . : / + =, which admits every convention in
// circulation — a UUID, plain hex, base64url, AWS X-Ray's "Root=1-…", and chi's
// own "hostname/base64-000001", whose slash is why this cannot be the narrower
// set it first looks like: rejecting it would mean one maniflex service calling
// another has its id regenerated at the far end, breaking the correlation the
// header exists to provide.
//
// What it excludes is whitespace, control characters, and the quoting and
// bracketing characters that make a logged or stored value awkward to read back.
func validRequestID(id string) bool {
	if len(id) > MaxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '/', c == '+', c == '=':
		default:
			return false
		}
	}
	return true
}
