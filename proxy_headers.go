package maniflex

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// proxyResolver derives the client address from forwarding headers, believing
// them only as far as Config.TrustedProxies says it may.
//
// An empty trusted set is the legacy mode Config.TrustProxyHeaders had on its
// own: the leftmost X-Forwarded-For entry from any peer. That is kept so
// enabling the flag keeps meaning what it meant, and warned about at startup —
// see collectRouterIssues.
type proxyResolver struct {
	// trusted are the proxies whose forwarding headers may be believed. Empty
	// means legacy mode.
	trusted []netip.Prefix

	// logger and unusableN back reportUnusable. Both nil in a resolver built for
	// a test or a validation pass, which silences the warning rather than
	// requiring every caller to supply a sink. The counter is a pointer so the
	// resolver stays copyable by value, as clientIP's receiver takes it.
	logger    *slog.Logger
	unusableN *atomic.Int64
}

// withLogger returns the resolver with a warning sink attached. Kept separate
// from newProxyResolver so parsing an allowlist stays a pure function that
// validation can call without a logger to hand.
func (r proxyResolver) withLogger(l *slog.Logger) proxyResolver {
	r.logger = l
	r.unusableN = new(atomic.Int64)
	return r
}

// newProxyResolver parses an allowlist of CIDRs and bare IP addresses.
//
// A bare address becomes a single-address prefix, so "10.0.0.9" and
// "10.0.0.9/32" mean the same thing. An entry that does not parse is an error
// rather than a skip: a typo in a security allowlist that silently narrows what
// is trusted would fail open on exactly the requests it was written to cover.
func newProxyResolver(cidrs []string) (proxyResolver, error) {
	var r proxyResolver
	for _, raw := range cidrs {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return proxyResolver{}, fmt.Errorf("empty entry")
		}
		if strings.Contains(entry, "/") {
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return proxyResolver{}, fmt.Errorf("%q is not a valid CIDR: %w", entry, err)
			}
			r.trusted = append(r.trusted, p.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return proxyResolver{}, fmt.Errorf("%q is not a valid IP address or CIDR: %w", entry, err)
		}
		addr = addr.Unmap()
		r.trusted = append(r.trusted, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return r, nil
}

// enabled reports whether an allowlist was configured.
func (r proxyResolver) enabled() bool { return len(r.trusted) > 0 }

// isTrusted reports whether addr is one of the declared proxies.
func (r proxyResolver) isTrusted(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, p := range r.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP resolves the client address for a request from peer (the TCP peer, in
// RemoteAddr form) and its forwarding headers. It reports false when nothing may
// be believed, in which case RemoteAddr is left as the peer.
//
// With an allowlist configured the chain is walked right-to-left. That direction
// is the whole security property: a proxy *appends* the address it saw, so the
// rightmost entries are the ones added by infrastructure and the leftmost is
// whatever the original client chose to send. Walking from the right past the
// hops we recognise stops at the first address a trusted proxy vouched for and
// the client could not have written.
func (r proxyResolver) clientIP(peer string, header http.Header) (string, bool) {
	if !r.enabled() {
		return r.legacyClientIP(header)
	}

	peerAddr, ok := parsePeer(peer)
	if !ok || !r.isTrusted(peerAddr) {
		// The connection did not come from a declared proxy, so this is a client
		// speaking for itself and its headers are its own to forge.
		return "", false
	}

	if raw := joinedHeader(header, "X-Forwarded-For"); raw != "" {
		ip, ok := r.walkForwardedFor(raw)
		if !ok {
			r.reportUnusable("X-Forwarded-For", raw)
		}
		return ip, ok
	}
	// No chain to walk. X-Real-IP can only be believed wholesale, which a
	// trusted peer has earned — but only when there is one of it. Two lines join
	// into something that is not an address, so the parse fails and nothing is
	// believed, which is the right answer: a single-valued header sent twice
	// means one of the two came from someone other than the proxy.
	if raw := joinedHeader(header, "X-Real-IP"); raw != "" {
		ip, ok := canonicalIP(raw)
		if !ok {
			r.reportUnusable("X-Real-IP", raw)
		}
		return ip, ok
	}
	// A trusted proxy that forwarded nothing is itself the client.
	return "", false
}

// reportUnusable warns that a declared proxy sent a forwarding header nothing
// could be read out of, so the request keeps the proxy's own address.
//
// It is worth a line because the symptom is otherwise unattributable: every
// client behind that proxy collapses into one identity, so per-IP rate limits
// stop separating anyone and audit records name the load balancer. Nothing in
// the request or the response says why.
//
// Only reached from the branch above, where the peer is a declared proxy — a
// direct client sending forwarding headers at a proxy-configured server is
// refused too, but that is the control working as intended and logging it would
// be a line per request. Throttled to once every logUnusableEvery occurrences,
// since a broken gateway produces one of these per request either way.
//
// The value is logged: it comes from a proxy the operator declared, and without
// it the message cannot be acted on. A client-forged entry can reach it, so it is
// truncated rather than passed through whole.
func (r proxyResolver) reportUnusable(header, raw string) {
	if r.logger == nil || r.unusableN == nil {
		return
	}
	if n := r.unusableN.Add(1); n != 1 && n%logUnusableEvery != 0 {
		return
	}
	const maxLogged = 200
	if len(raw) > maxLogged {
		raw = raw[:maxLogged] + "…"
	}
	r.logger.Warn("a trusted proxy sent a forwarding header no address could be read from, "+
		"so the request keeps the proxy's own address — every client behind it shares one "+
		"identity for rate limiting and audit",
		slog.String("header", header),
		slog.String("value", raw),
		slog.Int64("occurrences", r.unusableN.Load()),
		slog.String("hint", "check what the proxy writes into the chain; an entry it appends "+
			"must be an IP address, optionally with a port"))
}

// logUnusableEvery throttles reportUnusable after the first occurrence.
const logUnusableEvery = 1000

// walkForwardedFor returns the rightmost entry that is not a declared proxy.
//
// Entries are parsed as the walk reaches them, right to left, and only as far as
// it goes. That is the trust boundary made literal: the walk stops at the first
// address no trusted proxy vouched for, and everything to the left of that is
// whatever the client chose to send. Parsing the whole chain up front made that
// client-controlled tail able to fail a decision it has no part in — so a caller
// sending "unknown" as its first entry dropped itself into the load balancer's
// shared rate-limit bucket, and any gateway that writes a hop the parser does not
// recognise did the same to every client behind it (audit HTTP-7).
//
// A malformed entry *at* the decision point still fails the whole chain closed.
// Skipping it would let a client that sends "6.6.6.6, garbage" push the walk past
// the address the proxy added and back onto its own forgery, which is the attack
// this walk exists to stop.
func (r proxyResolver) walkForwardedFor(raw string) (string, bool) {
	parts := strings.Split(raw, ",")
	var leftmost netip.Addr
	for i := len(parts) - 1; i >= 0; i-- {
		addr, ok := parseForwardedAddr(parts[i])
		if !ok {
			return "", false
		}
		if !r.isTrusted(addr) {
			return addr.String(), true
		}
		leftmost = addr
	}
	// Every hop is a declared proxy, so none of them is the client. The leftmost
	// is the furthest the chain reaches and the only answer left.
	return leftmost.String(), true
}

// parseForwardedAddr reads one chain entry, which may carry a source port.
//
// Several gateways append one — Azure Front Door and Application Gateway write
// "203.0.113.7:54321", and an IPv6 hop arrives bracketed as "[2001:db8::1]:443".
// netip.ParseAddr rejects both, and rejecting a hop is not a neutral act here:
// it fails the chain closed to the proxy's own address, so every client behind
// such a gateway shares one identity for rate limiting and audit.
func parseForwardedAddr(raw string) (netip.Addr, bool) {
	return canonicalAddr(strings.TrimSpace(raw))
}

// canonicalAddr parses an address that may be bare or carry a port, in either
// family. SplitHostPort is tried first because it is the case that needs
// stripping; it fails on a bare address of either family — "no port" for IPv4,
// "too many colons" for IPv6 — leaving the original to parse as it stands.
func canonicalAddr(raw string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// legacyClientIP is the pre-allowlist behaviour: the leftmost X-Forwarded-For
// entry, then X-Real-IP, from any peer. A malformed X-Forwarded-For fails closed
// rather than falling back to the other client-controlled header.
func (r proxyResolver) legacyClientIP(header http.Header) (string, bool) {
	if raw := joinedHeader(header, "X-Forwarded-For"); raw != "" {
		first, _, _ := strings.Cut(raw, ",")
		return canonicalIP(first)
	}
	if raw := joinedHeader(header, "X-Real-IP"); raw != "" {
		return canonicalIP(raw)
	}
	return "", false
}

// joinedHeader is the field value of name: every line under it, in the order
// received, joined with commas.
//
// Header.Get returns only the first line, and a forwarding header often arrives
// on more than one. HAProxy's `option forwardfor` adds a line rather than
// extending the client's, as do several Java and Node proxies — so the client's
// own forgery is line one and the address the proxy actually saw is line two.
// Reading the first line alone hands the walk in walkForwardedFor a chain with
// nothing in it but the forgery, and it has no trusted hop to walk back past.
//
// RFC 9110 §5.3 defines the combined value as exactly this join, so this is what
// the header says rather than a reinterpretation of it.
func joinedHeader(header http.Header, name string) string {
	values := header.Values(name)
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	default:
		return strings.Join(values, ",")
	}
}

// parsePeer extracts the address from a RemoteAddr, which is "host:port" for a
// real connection but may be a bare address in tests or behind a rewriter — the
// same shapes a forwarded entry arrives in, so they share canonicalAddr.
func parsePeer(remoteAddr string) (netip.Addr, bool) {
	return canonicalAddr(remoteAddr)
}

// trustedProxyHeaders preserves the framework's RemoteAddr contract for
// IP-keyed middleware while avoiding chi's deprecated RealIP middleware.
//
// It is mounted when Config.TrustedProxies is non-empty or Config.TrustProxyHeaders
// is set. RemoteAddr is replaced with the resolved client address only when the
// resolver is willing to believe the headers; otherwise it stays the TCP peer.
func trustedProxyHeaders(r proxyResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if ip, ok := r.clientIP(req.RemoteAddr, req.Header); ok {
				req.RemoteAddr = ip
			}
			next.ServeHTTP(w, req)
		})
	}
}

func canonicalIP(raw string) (string, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return ip.Unmap().String(), true
}
