package maniflex

import (
	"net/http"
	"net/netip"
	"strings"
)

// trustedProxyHeaders preserves the framework's RemoteAddr contract for
// IP-keyed middleware while avoiding chi's deprecated RealIP middleware.
//
// This middleware is mounted only when Config.TrustProxyHeaders is true. It
// accepts X-Forwarded-For first, then X-Real-IP. If X-Forwarded-For is present
// but malformed, it fails closed instead of falling back to another
// client-controlled header.
func trustedProxyHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip, ok := trustedForwardedIP(r.Header); ok {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

func trustedForwardedIP(header http.Header) (string, bool) {
	if raw := header.Get("X-Forwarded-For"); raw != "" {
		first, _, _ := strings.Cut(raw, ",")
		return canonicalIP(first)
	}
	if raw := header.Get("X-Real-IP"); raw != "" {
		return canonicalIP(raw)
	}
	return "", false
}

func canonicalIP(raw string) (string, bool) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return ip.Unmap().String(), true
}
