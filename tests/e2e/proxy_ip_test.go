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
// keep-alive connection). Reading RemoteAddr is exactly what chi's RealIP
// rewrites when proxy headers are trusted, so this exercises the SEC-5 surface.
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
// per-IP rate limit by rotating X-Forwarded-For. RealIP is not mounted, so
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

// SEC-5: with TrustProxyHeaders on, chi's RealIP honours X-Forwarded-For, so
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
