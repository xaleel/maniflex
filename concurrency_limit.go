package maniflex

import (
	"io"
	"net/http"
	"strconv"
)

// serverBusyRetryAfter is what a shed request is told to wait, in seconds.
//
// One second, where a rejected export is told thirty: an export is slow by
// nature, so a sub-second retry would just spin, but the concurrency cap sheds
// ordinary requests during a burst that a healthy server clears in well under a
// second. Telling those clients to wait thirty would turn a momentary spike
// into a half-minute outage for everyone caught in it.
const serverBusyRetryAfter = 1

// concurrencyLimit bounds how many requests may be in flight at once, answering
// 503 rather than queueing when the limit is reached.
//
// It does not wait for a slot, for the same reason acquireExportSlot does not:
// a request that would have to queue is better told to come back than left
// holding a connection, a goroutine and its parsed body for an unbounded time.
//
// Without it the connection pool is the de facto concurrency limit, and it is
// one that fails by queueing — a burst does not get refused, it piles up
// waiting for a connection until QueryTimeout fires, so latency climbs for
// every request rather than being paid by the ones that are shed. This turns
// that slow collapse into fast rejection the caller can retry.
//
// Requests to a path in exempt are never shed. That is for the probe endpoints
// (see mountedProbePaths): shedding one reports the process dead or unfit for
// traffic when it is merely busy, so the orchestrator restarts it or takes it
// out of the load balancer — escalating a spike this limit was absorbing into
// an outage, across every replica at once since they saturate together. A probe
// is cheap enough to answer anyway: /live returns a constant and the other two
// collapse onto one dependency check through probeFlight.
func concurrencyLimit(n int, exempt map[string]struct{}) func(http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := exempt[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}
			select {
			case sem <- struct{}{}:
				// Deferred, so a panicking handler returns its slot. Leaking one
				// per panic would shrink capacity until the server served
				// nothing but 503s.
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
			default:
				writeServerBusy(w)
			}
		})
	}
}

// writeServerBusy answers a request that found every slot taken. Written here
// rather than through ctx.Abort because the point of the limit is to refuse
// before the pipeline — and its DB work — runs at all.
func writeServerBusy(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(serverBusyRetryAfter))
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w,
		`{"error":{"code":"SERVER_BUSY","message":"too many concurrent requests; retry shortly"}}`)
}
