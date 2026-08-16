package e2e_test

import (
	"net"
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ProxyTarget is a throwaway model for the trusted-proxy / IP-spoofing tests.
type ProxyTarget struct {
	maniflex.BaseModel
	Payload string `json:"payload" db:"payload" mfx:"required"`
}

// ipKeyRateLimit registers a per-IP create limit keyed on the client-IP portion
// of RemoteAddr (dropping the ephemeral TCP port so the bucket is stable across a
// keep-alive connection). Reading RemoteAddr is exactly what the trusted-proxy
// resolver rewrites, so this exercises the SEC-5 surface.
func ipKeyRateLimit(srv *maniflex.Server, perMinute int) {
	srv.Pipeline.DB.Register(
		dbmw.RateLimit(dbmw.RateLimitConfig{
			RequestsPerMinute: perMinute,
			KeyFunc: func(ctx *maniflex.ServerContext) string {
				host, _, err := net.SplitHostPort(ctx.Request.RemoteAddr)
				if err != nil {
					return ctx.Request.RemoteAddr
				}
				return host
			},
		}),
		maniflex.ForModel("ProxyTarget"),
		maniflex.ForOperation(maniflex.OpCreate),
	)
}

// SEC-5: with TrustProxyHeaders off (the default), a client cannot escape a
// per-IP rate limit by rotating X-Forwarded-For. Proxy resolution is not mounted, so
// RemoteAddr stays the real TCP peer and every spoofed request shares one bucket.
func TestTrustProxyHeaders_OffRejectsSpoofedXFF(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models:     []any{ProxyTarget{}},
		Middleware: func(srv *maniflex.Server) { ipKeyRateLimit(srv, 3) },
	})

	spoof := func(ip string) *testutil.Response {
		return s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": ip})
	}

	// Three requests, each claiming a different client IP, exhaust the single
	// real-peer bucket regardless of the forged header.
	spoof("10.0.0.1").AssertStatus(http.StatusCreated)
	spoof("10.0.0.2").AssertStatus(http.StatusCreated)
	spoof("10.0.0.3").AssertStatus(http.StatusCreated)
	// A fourth spoofed IP must still be rate-limited — the header was ignored.
	spoof("10.0.0.4").AssertStatus(http.StatusTooManyRequests)
}

// SEC-5: with TrustProxyHeaders on, the router honours X-Forwarded-For, so
// each forwarded IP gets its own bucket. This documents why the switch must only
// be enabled behind a proxy that strips inbound XFF — directly internet-facing it
// would let a client spoof its way around the limit (the case above prevents).
func TestTrustProxyHeaders_OnHonoursXFF(t *testing.T) {
	s := testutil.NewServer(t, testutil.Options{
		Models:            []any{ProxyTarget{}},
		TrustProxyHeaders: true,
		Middleware:        func(srv *maniflex.Server) { ipKeyRateLimit(srv, 3) },
	})

	spoof := func(ip string) *testutil.Response {
		return s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": ip})
	}

	// Distinct forwarded IPs are distinct buckets, so all succeed even though
	// they collectively exceed the per-IP limit of 3.
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"} {
		spoof(ip).AssertStatus(http.StatusCreated)
	}
}

// ── S1: TrustedProxies ────────────────────────────────────────────────────────

// proxyTargetServer builds the rate-limited fixture with a caller-supplied proxy
// configuration, using testutil's Config escape hatch rather than growing an
// option per field.
func proxyTargetServer(t *testing.T, cfg func(*maniflex.Config)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models:     []any{ProxyTarget{}},
		Config:     cfg,
		Middleware: func(srv *maniflex.Server) { ipKeyRateLimit(srv, 3) },
	})
}

// S1. With an allowlist configured, a caller that is not one of the declared
// proxies gets no say in its own address. The test client connects from
// 127.0.0.1, which this allowlist deliberately excludes, so every forged header
// is ignored and the requests share one bucket.
func TestTrustedProxies_UntrustedPeerCannotSpoof(t *testing.T) {
	s := proxyTargetServer(t, func(c *maniflex.Config) {
		c.TrustedProxies = []string{"10.0.0.0/8"} // not the test client
	})

	spoof := func(ip string) *testutil.Response {
		return s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": ip})
	}

	spoof("203.0.113.1").AssertStatus(http.StatusCreated)
	spoof("203.0.113.2").AssertStatus(http.StatusCreated)
	spoof("203.0.113.3").AssertStatus(http.StatusCreated)
	// A fourth forged address must still be limited: the peer was never entitled
	// to speak for anyone, so all four requests keyed on the same real address.
	spoof("203.0.113.4").AssertStatus(http.StatusTooManyRequests)
}

// S1, the headline. The peer *is* a declared proxy, so its X-Forwarded-For is
// believed — but only from the right. A real proxy appends the address it saw,
// so the rightmost entry is the truth and anything to its left is whatever the
// original client chose to send. Holding the rightmost fixed means every request
// is really the same client, however the forged prefix varies.
func TestTrustedProxies_ForgedLeftmostSharesTheRealClientsBucket(t *testing.T) {
	s := proxyTargetServer(t, func(c *maniflex.Config) {
		c.TrustedProxies = []string{"127.0.0.1/32", "::1/128"} // the test client
	})

	forge := func(forged string) *testutil.Response {
		return s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": forged + ", 203.0.113.99"})
	}

	forge("6.6.6.1").AssertStatus(http.StatusCreated)
	forge("6.6.6.2").AssertStatus(http.StatusCreated)
	forge("6.6.6.3").AssertStatus(http.StatusCreated)
	// All four resolved to 203.0.113.99 — the address the trusted proxy vouched
	// for — so rotating the forged prefix bought no extra quota.
	forge("6.6.6.4").AssertStatus(http.StatusTooManyRequests)
}

// The other half of the contract: the address a trusted proxy did vouch for is
// honoured, so genuinely distinct clients still get distinct buckets.
func TestTrustedProxies_ForwardedClientIsHonoured(t *testing.T) {
	s := proxyTargetServer(t, func(c *maniflex.Config) {
		c.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	})

	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4"} {
		s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": ip}).AssertStatus(http.StatusCreated)
	}
}

// The hazard TrustedProxies exists to remove, pinned so it cannot be mistaken
// for a fixed behaviour: with the bare TrustProxyHeaders flag the leftmost entry
// is believed, so the same forgery that bought nothing above buys a fresh bucket
// per request. This is why the flag warns at startup and fails under Strict.
func TestTrustProxyHeaders_LegacyModeStillTrustsTheForgedLeftmost(t *testing.T) {
	s := proxyTargetServer(t, func(c *maniflex.Config) {
		c.TrustProxyHeaders = true // no allowlist
	})

	forge := func(forged string) *testutil.Response {
		return s.POST("/proxy_targets", map[string]any{"payload": "x"},
			map[string]string{"X-Forwarded-For": forged + ", 203.0.113.99"})
	}

	for _, f := range []string{"6.6.6.1", "6.6.6.2", "6.6.6.3", "6.6.6.4", "6.6.6.5"} {
		forge(f).AssertStatus(http.StatusCreated)
	}
}
