package maniflex

import (
	"io"
	"net/http"
	"time"
)

// bodyReadDeadline bounds how long the server waits for the next chunk of a
// request body, refreshing the bound on every read.
//
// It closes the body-phase gap: ReadHeaderTimeout stops at the headers, and the
// body size cap counts bytes rather than seconds, so between the two a client
// could announce a Content-Length well inside the limit and then fall silent,
// holding a connection, a goroutine and a file descriptor indefinitely.
//
// The bound is armed when the body is wrapped, refreshed immediately before each
// underlying read, and cleared once the body reaches EOF. Three things follow,
// and all three matter:
//
//   - A slow upload that keeps making progress is never cut off, because every
//     read starts its own fresh bound. That is what makes this safe to enable by
//     default where Config.ReadTimeout — a deadline over the whole request — is
//     not, and it is the same semantics as nginx's client_body_timeout.
//   - A handler that never touches the body is covered too. It has to be: net/http
//     drains an unread body so the connection can be reused, and that drain is a
//     read from the same client that went silent. It runs after the handler on a
//     small response and mid-handler once the response buffer flushes, so neither
//     arming inside Read nor re-arming after the handler reaches both — only a
//     deadline standing from the outset does. It also reads net/http's own body
//     rather than this wrapper, since a request copied by r.WithContext carries
//     the replacement no further than the copy; the bound therefore has to live
//     on the connection, not in Read.
//   - No deadline survives EOF, so a handler that reads its body and then streams
//     for minutes is untouched. Before EOF one can stand harmlessly: net/http only
//     starts the background read that would trip it once the body is exhausted
//     (see requestBodyRemains in net/http), and tripping it mid-response is
//     precisely the failure that keeps WriteTimeout unset.
//
// A connection whose deadline cannot be set — HTTP/2, or an unwrapped
// ResponseWriter — is left as it was rather than refused: this is a hardening
// measure, and failing a request over its absence would be the larger harm.
// effectiveBodyReadTimeout resolves the idle body bound actually in force.
//
// Zero means no wrapping: either the operator disabled it with a negative value
// — the ReadHeaderTimeout/IdleTimeout convention — or they set Config.ReadTimeout,
// a deadline over the whole request. Refreshing a bound on every read would
// silently extend past that stricter one the caller chose deliberately.
func effectiveBodyReadTimeout(cfg *Config) time.Duration {
	if cfg.ReadTimeout > 0 || cfg.BodyReadTimeout <= 0 {
		return 0
	}
	return cfg.BodyReadTimeout
}

func bodyReadDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				b := &deadlineBody{
					body: r.Body,
					rc:   http.NewResponseController(w),
					d:    d,
				}
				// Armed before the handler runs, not on first read: a handler
				// that answers without reading still leaves net/http a body to
				// drain from a client that may already have gone silent.
				b.arm()
				r.Body = b
			}
			next.ServeHTTP(w, r)
		})
	}
}

// deadlineBody wraps a request body so each read is individually bounded.
type deadlineBody struct {
	body io.ReadCloser
	rc   *http.ResponseController
	d    time.Duration

	// unsupported records that the connection refused a deadline, so the
	// remaining reads of a large body do not each retry a call that cannot work.
	unsupported bool
}

// arm bounds the next read from the client, whoever makes it: this body, or the
// drain net/http runs over whatever the handler left unread. A connection that
// refuses a deadline is recorded so the remaining reads of a large body do not
// each retry a call that cannot work.
func (b *deadlineBody) arm() {
	if b.unsupported {
		return
	}
	if err := b.rc.SetReadDeadline(time.Now().Add(b.d)); err != nil {
		b.unsupported = true
	}
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	b.arm()
	n, err := b.body.Read(p)
	if err == io.EOF {
		// A clean end of body: there is nothing left for net/http to drain, so
		// the server has stopped waiting on this client for good. Clear it —
		// one left in force would go on to bound whatever the handler does
		// next, and net/http, which starts its background read at exactly this
		// point, trips it on that read and cancels a long response mid-flight.
		// That is the failure that keeps WriteTimeout unset.
		b.rc.SetReadDeadline(time.Time{}) //nolint:errcheck // best-effort clear
	}
	// Every other outcome leaves the bound standing. A failed read leaves an
	// expired one, and a partial read the fresh one armed above; either way the
	// body is not exhausted, so net/http will drain the rest from the same
	// client after the handler returns. Clearing here would hand that drain an
	// unbounded read and park the connection for the whole IdleTimeout.
	return n, err
}

func (b *deadlineBody) Close() error { return b.body.Close() }
