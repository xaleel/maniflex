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
// The deadline is set immediately before each underlying read and cleared as
// soon as that read returns, so one is in force only while the server is
// actually waiting on the client. Two things follow, and both matter:
//
//   - A slow upload that keeps making progress is never cut off, because every
//     read starts its own fresh bound. That is what makes this safe to enable by
//     default where Config.ReadTimeout — a deadline over the whole request — is
//     not, and it is the same semantics as nginx's client_body_timeout.
//   - No deadline survives into the response, so a handler that reads its body
//     and then streams for minutes is untouched. Leaving one set would have the
//     connection's background read trip it mid-response and cancel the request,
//     which is precisely the failure that keeps WriteTimeout unset.
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
				r.Body = &deadlineBody{
					body: r.Body,
					rc:   http.NewResponseController(w),
					d:    d,
				}
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

func (b *deadlineBody) Read(p []byte) (int, error) {
	if b.unsupported {
		return b.body.Read(p)
	}
	if err := b.rc.SetReadDeadline(time.Now().Add(b.d)); err != nil {
		b.unsupported = true
		return b.body.Read(p)
	}
	n, err := b.body.Read(p)
	if err == nil || err == io.EOF {
		// Progress, or a clean end of body. Either way the server has stopped
		// waiting on the client, so the deadline is cleared: one left in force
		// would go on to bound whatever the handler does next, and net/http
		// trips it on the connection's background read — cancelling a long
		// response mid-flight, which is the failure that keeps WriteTimeout
		// unset.
		b.rc.SetReadDeadline(time.Time{}) //nolint:errcheck // best-effort clear
	}
	// A failed read deliberately leaves the expired deadline in place. After the
	// handler returns net/http drains what is left of an unread body so the
	// connection can be reused, and that drain is another read from the same
	// client that just stopped responding — clearing here would hand it an
	// unbounded one and park the connection for the whole IdleTimeout.
	return n, err
}

func (b *deadlineBody) Close() error { return b.body.Close() }
