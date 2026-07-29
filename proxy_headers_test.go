package maniflex

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		header     http.Header
		remoteAddr string
		want       string
	}{
		{
			name:       "forwarded_for_uses_first_address",
			header:     http.Header{"X-Forwarded-For": []string{" 203.0.113.7, 10.0.0.2"}},
			remoteAddr: "127.0.0.1:1234",
			want:       "203.0.113.7",
		},
		{
			name:       "real_ip_is_fallback",
			header:     http.Header{"X-Real-Ip": []string{"2001:db8::1"}},
			remoteAddr: "127.0.0.1:1234",
			want:       "2001:db8::1",
		},
		{
			name: "forwarded_for_takes_precedence",
			header: http.Header{
				"X-Forwarded-For": []string{"203.0.113.8"},
				"X-Real-Ip":       []string{"203.0.113.9"},
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "203.0.113.8",
		},
		{
			name: "malformed_forwarded_for_fails_closed",
			header: http.Header{
				"X-Forwarded-For": []string{"not-an-ip"},
				"X-Real-Ip":       []string{"203.0.113.9"},
			},
			remoteAddr: "127.0.0.1:1234",
			want:       "127.0.0.1:1234",
		},
		{
			name:       "true_client_ip_is_not_trusted",
			header:     http.Header{"True-Client-Ip": []string{"203.0.113.10"}},
			remoteAddr: "127.0.0.1:1234",
			want:       "127.0.0.1:1234",
		},
		{
			name:       "ipv4_mapped_ipv6_is_canonicalized",
			header:     http.Header{"X-Real-Ip": []string{"::ffff:203.0.113.11"}},
			remoteAddr: "127.0.0.1:1234",
			want:       "203.0.113.11",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header = tc.header
			req.RemoteAddr = tc.remoteAddr

			var got string
			trustedProxyHeaders(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			})).ServeHTTP(httptest.NewRecorder(), req)

			if got != tc.want {
				t.Errorf("RemoteAddr = %q, want %q", got, tc.want)
			}
		})
	}
}
