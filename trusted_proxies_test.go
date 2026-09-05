package maniflex

// Audit S1 — with TrustProxyHeaders on, the *leftmost* X-Forwarded-For entry was
// trusted from any peer. The leftmost entry is the one an attacker controls: a
// proxy appends the address it saw, so a client that sends its own XFF header
// has its forgery sit in front of the real address the proxy adds. That defeats
// per-IP rate limiting, mis-scopes idempotency keys, and poisons read-audit
// records — and there was no way to say which peers may be believed.
//
// Config.TrustedProxies is that allowlist: headers are honoured only when the
// TCP peer is one of the named proxies, and the chain is walked right-to-left
// past trusted hops so the first address a trusted proxy did not vouch for wins.

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func mustResolver(t *testing.T, cidrs ...string) proxyResolver {
	t.Helper()
	r, err := newProxyResolver(cidrs)
	if err != nil {
		t.Fatalf("newProxyResolver(%v): %v", cidrs, err)
	}
	return r
}

// The attack the allowlist exists to stop. A client at 203.0.113.7 sends a
// forged XFF; the load balancer appends the address it actually saw, so the
// forgery ends up on the left and the truth on the right. Trusting the leftmost
// entry hands the attacker whatever address it asked for.
func TestProxyResolver_ForgedLeftmostEntryIsIgnored(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdr("X-Forwarded-For", "6.6.6.6, 203.0.113.7"))

	if !ok {
		t.Fatal("a trusted peer's X-Forwarded-For was not honoured at all")
	}
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want %q — the leftmost entry is client-controlled, "+
			"so the walk must start from the right and stop at the first address no "+
			"trusted proxy vouched for", got, "203.0.113.7")
	}
}

// A peer that is not a declared proxy gets no say in its own address, however
// many headers it sends.
func TestProxyResolver_UntrustedPeerIsNotHonoured(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	if got, ok := r.clientIP("203.0.113.7:51000",
		hdr("X-Forwarded-For", "6.6.6.6")); ok {
		t.Errorf("X-Forwarded-For was honoured from an untrusted peer (got %q); a "+
			"directly-connected client must not be able to forge its address", got)
	}
	if got, ok := r.clientIP("203.0.113.7:51000",
		hdr("X-Real-IP", "6.6.6.6")); ok {
		t.Errorf("X-Real-IP was honoured from an untrusted peer (got %q)", got)
	}
}

// Two trusted hops in front of the server: the walk skips both and returns the
// address the outermost proxy vouched for.
func TestProxyResolver_WalksPastTrustedHops(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8", "192.168.1.5")

	got, ok := r.clientIP("10.0.0.9:443",
		hdr("X-Forwarded-For", "6.6.6.6, 203.0.113.7, 192.168.1.5"))

	if !ok || got != "203.0.113.7" {
		t.Errorf("client IP = %q (ok=%v), want %q — 192.168.1.5 is a declared proxy "+
			"and must be walked past, and the forged leftmost entry ignored",
			got, ok, "203.0.113.7")
	}
}

// Every entry is a declared proxy, so no hop is the client. The leftmost is the
// furthest thing the chain knows about; there is nothing better to report.
func TestProxyResolver_AllHopsTrustedFallsBackToLeftmost(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdr("X-Forwarded-For", "10.0.0.4, 10.0.0.5"))

	if !ok || got != "10.0.0.4" {
		t.Errorf("client IP = %q (ok=%v), want %q", got, ok, "10.0.0.4")
	}
}

// X-Real-IP carries no chain, so it can only ever be believed wholesale — and
// only from a peer that is allowed to speak for someone else.
func TestProxyResolver_XRealIPFromTrustedPeer(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443", hdr("X-Real-IP", "203.0.113.7"))
	if !ok || got != "203.0.113.7" {
		t.Errorf("client IP = %q (ok=%v), want %q", got, ok, "203.0.113.7")
	}
}

// A trusted peer that forwards nothing is the client.
func TestProxyResolver_TrustedPeerWithNoHeadersIsUnchanged(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	if got, ok := r.clientIP("10.0.0.9:443", hdr()); ok {
		t.Errorf("RemoteAddr was rewritten to %q with no forwarding headers present", got)
	}
}

// A malformed entry must not be silently skipped over to reach a forgeable one:
// the chain has stopped making sense, so nothing is believed.
func TestProxyResolver_MalformedEntryFailsClosed(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	if got, ok := r.clientIP("10.0.0.9:443",
		hdr("X-Forwarded-For", "6.6.6.6, not-an-ip")); ok {
		t.Errorf("a malformed X-Forwarded-For entry was walked past to reach %q; a "+
			"chain that does not parse must fail closed", got)
	}
}

// Legacy mode: no allowlist configured, so TrustProxyHeaders keeps its old
// meaning exactly — the leftmost entry, from any peer. Existing deployments must
// not change behaviour under them.
func TestProxyResolver_NoAllowlistKeepsLegacyLeftmost(t *testing.T) {
	r := mustResolver(t)

	got, ok := r.clientIP("203.0.113.7:51000",
		hdr("X-Forwarded-For", "6.6.6.6, 198.51.100.2"))
	if !ok || got != "6.6.6.6" {
		t.Errorf("legacy mode client IP = %q (ok=%v), want %q — behaviour must be "+
			"unchanged when no allowlist is configured", got, ok, "6.6.6.6")
	}
}

// IPv6, and the canonical form the rest of the framework keys on.
func TestProxyResolver_IPv6AndCanonicalisation(t *testing.T) {
	r := mustResolver(t, "2001:db8::/32")

	got, ok := r.clientIP("[2001:db8::1]:443",
		hdr("X-Forwarded-For", "::ffff:203.0.113.7"))
	if !ok || got != "203.0.113.7" {
		t.Errorf("client IP = %q (ok=%v), want %q — a v4-mapped address must be "+
			"unmapped so it keys the same as the v4 form", got, ok, "203.0.113.7")
	}
}

func TestNewProxyResolver_RejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "10.0.0.0/99", "10.0.0.0/", ""} {
		if _, err := newProxyResolver([]string{bad}); err == nil {
			t.Errorf("newProxyResolver(%q) accepted an unparseable entry; a typo in a "+
				"security allowlist must not boot", bad)
		}
	}
}

func TestNewProxyResolver_AcceptsBareIPAndCIDR(t *testing.T) {
	if _, err := newProxyResolver([]string{"10.0.0.9", "192.168.0.0/16", "::1"}); err != nil {
		t.Errorf("newProxyResolver rejected valid entries: %v", err)
	}
}

// hdrLines builds a header carrying name on several field lines, the way a proxy
// that adds its own leaves one alongside the client's.
func hdrLines(name string, values ...string) http.Header {
	h := http.Header{}
	for _, v := range values {
		h.Add(name, v)
	}
	return h
}

// The same forgery as TestProxyResolver_ForgedLeftmostEntryIsIgnored, delivered
// on two field lines instead of one. HAProxy's `option forwardfor` adds a line
// rather than extending the client's, so the client's value is line one and the
// address the proxy saw is line two. Reading only the first line hands the walk a
// chain containing nothing but the forgery, with no trusted hop to walk back past
// — so the allowlist mode, which exists to stop exactly this, returns the
// attacker's address (audit HTTP-2).
func TestProxyResolver_ForgedFirstLineIsIgnored(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdrLines("X-Forwarded-For", "6.6.6.6", "203.0.113.7"))

	if !ok {
		t.Fatal("a trusted peer's X-Forwarded-For was not honoured at all")
	}
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want %q — repeated field lines are one chain in "+
			"order (RFC 9110 §5.3), so the walk must see every line, not the first",
			got, "203.0.113.7")
	}
}

// A chain split across lines is still one chain: the walk has to cross the line
// boundary to reach the trusted hop and stop in front of it.
func TestProxyResolver_ChainSplitAcrossLinesWalksPastTrustedHops(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdrLines("X-Forwarded-For", "6.6.6.6", "203.0.113.7, 10.0.0.4"))

	if !ok {
		t.Fatal("the split chain was not honoured at all")
	}
	if got != "203.0.113.7" {
		t.Errorf("client IP = %q, want %q", got, "203.0.113.7")
	}
}

// X-Real-IP names one address. Two lines mean one of them came from someone
// other than the proxy, and there is nothing in the header to say which — so
// nothing is believed and RemoteAddr stays the peer, as it does for a malformed
// X-Forwarded-For.
func TestProxyResolver_MultipleXRealIPLinesFailClosed(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdrLines("X-Real-Ip", "6.6.6.6", "203.0.113.7"))

	if ok {
		t.Errorf("client IP = %q from two X-Real-IP lines; a single-valued header "+
			"sent twice cannot be believed", got)
	}
}

// One line is still believed wholesale — the fail-closed rule above must not cost
// the ordinary case.
func TestProxyResolver_SingleXRealIPLineIsStillBelieved(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443", hdrLines("X-Real-Ip", "203.0.113.7"))

	if !ok || got != "203.0.113.7" {
		t.Errorf("client IP = %q ok=%v, want 203.0.113.7", got, ok)
	}
}

// Legacy mode is unchanged by the join. It takes the leftmost entry from any
// peer, and the leftmost of the joined chain is the same value Get returned from
// the first line — which is the client's own, as that mode has always documented.
func TestProxyResolver_LegacyModeUnaffectedByExtraLines(t *testing.T) {
	got, ok := proxyResolver{}.clientIP("127.0.0.1:1234",
		hdrLines("X-Forwarded-For", "6.6.6.6", "203.0.113.7"))

	if !ok || got != "6.6.6.6" {
		t.Errorf("legacy client IP = %q ok=%v, want 6.6.6.6 — adding an allowlist "+
			"must not change what the allowlist-free mode does", got, ok)
	}
}

// Several gateways append the source port to the entry they add — Azure Front
// Door and Application Gateway do, as do some F5 and Java stacks. Rejecting such
// a hop is not neutral: it fails the chain closed to the proxy's own address, so
// every client behind that gateway shares one identity for rate limiting and
// audit (audit HTTP-7).
func TestProxyResolver_EntryCarryingAPortIsRead(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	for _, tc := range []struct{ chain, want string }{
		{"203.0.113.7:54321, 10.0.0.9", "203.0.113.7"},
		{"[2001:db8::1]:54321, 10.0.0.9", "2001:db8::1"},
		{"203.0.113.7, 10.0.0.9", "203.0.113.7"}, // bare, unchanged
		{"2001:db8::1, 10.0.0.9", "2001:db8::1"}, // bare v6, unchanged
	} {
		got, ok := r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", tc.chain))
		if !ok || got != tc.want {
			t.Errorf("chain %q → %q ok=%v, want %q", tc.chain, got, ok, tc.want)
		}
	}
}

// The walk stops at the first address no trusted proxy vouched for; everything
// left of that is what the client chose to send and is never consulted. Parsing
// the whole chain up front let that tail fail a decision it has no part in — so
// a caller could send "unknown" and put itself in the load balancer's shared
// bucket, escaping per-IP limits and leaving audit records naming the balancer.
func TestProxyResolver_JunkLeftOfTheDecisionPointIsIgnored(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	for _, chain := range []string{
		"unknown, 203.0.113.7, 10.0.0.9",
		"_hidden, 203.0.113.7, 10.0.0.9",
		"!!!, more junk, 203.0.113.7, 10.0.0.9",
	} {
		got, ok := r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", chain))
		if !ok || got != "203.0.113.7" {
			t.Errorf("chain %q → %q ok=%v, want 203.0.113.7 — the walk stops before "+
				"reaching the client-controlled entries", chain, got, ok)
		}
	}
}

// The property the lazy walk must not cost: junk *at* the decision point fails
// the chain closed. Skipping it would push the walk past the address the proxy
// added and onto the client's forgery, which is what the walk exists to stop.
func TestProxyResolver_JunkAtTheDecisionPointFailsClosed(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	for _, chain := range []string{
		"6.6.6.6, garbage, 10.0.0.9", // forgery hiding behind junk
		"garbage, 10.0.0.9",
		"6.6.6.6, unknown, 10.0.0.9",
	} {
		if got, ok := r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", chain)); ok {
			t.Errorf("chain %q → %q; an unreadable entry at the decision point must fail "+
				"closed or the walk lands on the client's own value", chain, got)
		}
	}
}

// A proxy that appends a port to its own hop is still recognised as trusted, so
// the walk continues past it rather than stopping on it.
func TestProxyResolver_TrustedHopWithAPortIsStillTrusted(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")

	got, ok := r.clientIP("10.0.0.9:443",
		hdr("X-Forwarded-For", "203.0.113.7, 10.0.0.4:5000, 10.0.0.9:443"))

	if !ok || got != "203.0.113.7" {
		t.Errorf("client IP = %q ok=%v, want 203.0.113.7", got, ok)
	}
}

// A chain nothing can be read from is worth a line: every client behind that
// proxy collapses into one identity, and no request or response says why.
func TestProxyResolver_UnusableChainIsLogged(t *testing.T) {
	var buf bytes.Buffer
	r := mustResolver(t, "10.0.0.0/8").
		withLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if _, ok := r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", "garbage, 10.0.0.9")); ok {
		t.Fatal("precondition: the chain was believed")
	}
	if !strings.Contains(buf.String(), "X-Forwarded-For") {
		t.Errorf("nothing logged for an unusable chain: %q", buf.String())
	}
}

// A direct client sending forwarding headers at a proxy-configured server is
// refused too — that is the control working, and logging it would be a line per
// request.
func TestProxyResolver_UntrustedPeerIsNotLogged(t *testing.T) {
	var buf bytes.Buffer
	r := mustResolver(t, "10.0.0.0/8").
		withLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if _, ok := r.clientIP("203.0.113.7:443", hdr("X-Forwarded-For", "garbage")); ok {
		t.Fatal("precondition: an untrusted peer was believed")
	}
	if buf.Len() != 0 {
		t.Errorf("a direct client produced a warning: %q", buf.String())
	}
}

// A broken gateway sends one of these per request; the log must not become the
// load.
func TestProxyResolver_UnusableChainLoggingIsThrottled(t *testing.T) {
	var buf bytes.Buffer
	r := mustResolver(t, "10.0.0.0/8").
		withLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))

	for range 50 {
		r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", "garbage, 10.0.0.9")) //nolint:errcheck
	}
	if n := strings.Count(buf.String(), "forwarding header"); n != 1 {
		t.Errorf("logged %d times for 50 requests, want 1", n)
	}
}

// A resolver with no sink attached must not panic — validation builds one that
// way.
func TestProxyResolver_NoLoggerIsSilentNotFatal(t *testing.T) {
	r := mustResolver(t, "10.0.0.0/8")
	if _, ok := r.clientIP("10.0.0.9:443", hdr("X-Forwarded-For", "garbage, 10.0.0.9")); ok {
		t.Error("the chain was believed")
	}
}
