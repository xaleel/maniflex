package maniflex

import "net"

// ClientIP returns the address the request came from, with any port removed.
//
// Go fills Request.RemoteAddr with "IP:port", and the port is the client's
// ephemeral one — a different value on every TCP connection. Anything that
// buckets requests by caller must key on the address alone, or a client that
// opens a fresh connection per request gets a fresh bucket each time and the
// limit, scope or quota never applies to it.
//
// Behind a proxy this is the real client only when the server is resolving
// forwarding headers — see Config.TrustedProxies. The resolver rewrites
// RemoteAddr to a bare address, which has no port to remove and is returned
// unchanged.
//
// It is exported because the same address is what a custom KeyFunc needs:
// db.RateLimitConfig.KeyFunc, idempotency.Config.KeyFunc, and any other
// per-caller bucket should reach for this rather than RemoteAddr.
func (c *ServerContext) ClientIP() string {
	if c.Request == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
		return host
	}
	return c.Request.RemoteAddr
}
