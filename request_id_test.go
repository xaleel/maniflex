package maniflex

// Audit HTTP-8 — chi's RequestID middleware adopts X-Request-Id from the client
// verbatim and unbounded. That value is echoed in the response, added to every
// log line the request produces, and persisted: into audit records, and into the
// request_id column of a versioned model's history table. A 60 KB client-chosen
// string reached all of them, once per request.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type requestIDModel struct {
	BaseModel
	Title string `json:"title"`
}

func requestIDServer(t *testing.T, logTo *bytes.Buffer) *httptest.Server {
	t.Helper()
	cfg := Config{}
	if logTo != nil {
		cfg.Logger = slog.New(slog.NewTextHandler(logTo, nil))
	}
	srv := New(cfg)
	srv.MustRegister(requestIDModel{})
	h, err := srv.handler()
	if err != nil {
		t.Fatalf("handler(): %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

// sendRequestID returns the id the server actually used.
func sendRequestID(t *testing.T, ts *httptest.Server, id string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/request_id_models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-Id", id)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send %.30q: %v", id, err)
	}
	defer resp.Body.Close()
	return resp.Header.Get("X-Request-Id")
}

// Every convention in circulation has to survive, or the header stops correlating
// anything. chi's own format is the one that catches a charset chosen too
// narrowly: it carries a slash, so a maniflex service forwarding its id to
// another maniflex service would have it regenerated at the far end.
func TestRequestID_AcceptedFormats(t *testing.T) {
	t.Parallel()
	ts := requestIDServer(t, nil)

	for _, tc := range []struct{ name, id string }{
		{"uuid", "550e8400-e29b-41d4-a716-446655440000"},
		{"chi", "web-01/aT3nG2BYG8-000001"},
		{"hex", "9f8c2b1a4d6e"},
		{"xray", "Root=1-5759e988-bd862e3fe1be46a994272793"},
		{"base64url", "q1w2e3r4_t5y6-u7i8=="},
		{"dotted", "svc.web.1:42"},
		{"max_length", strings.Repeat("a", MaxRequestIDLength)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendRequestID(t, ts, tc.id); got != tc.id {
				t.Errorf("id %q was replaced with %q; this format is in real use and "+
					"regenerating it breaks the correlation the header exists for", tc.id, got)
			}
		})
	}
}

func TestRequestID_RejectedValuesAreReplaced(t *testing.T) {
	t.Parallel()
	ts := requestIDServer(t, nil)

	for _, tc := range []struct{ name, id string }{
		{"too_long", strings.Repeat("a", MaxRequestIDLength+1)},
		{"huge", strings.Repeat("A", 60_000)},
		{"quotes", `x" evil="yes`},
		{"spaces", "id with spaces"},
		{"unicode", strings.Repeat("💥", 40)},
		{"braces", "{\"forged\":true}"},
		{"backslash", `a\b`},
		{"blank", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendRequestID(t, ts, tc.id)
			if got == tc.id {
				t.Errorf("id %.40q was adopted verbatim (%d bytes)", tc.id, len(got))
			}
			if got == "" {
				t.Error("no id was used at all; a rejected header must be replaced, not dropped")
			}
			if !validRequestID(got) {
				t.Errorf("the substituted id %q does not itself pass validation", got)
			}
		})
	}
}

// The size half of the finding, at the place it actually costs: the log line.
func TestRequestID_OversizedValueDoesNotReachTheLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	ts := requestIDServer(t, &buf)

	big := strings.Repeat("Z", 50_000)
	sendRequestID(t, ts, big)

	if strings.Contains(buf.String(), big) {
		t.Errorf("the %d-byte client value reached the log", len(big))
	}
	if buf.Len() > 4096 {
		t.Errorf("one request produced %d bytes of log for a 50 KB request id", buf.Len())
	}
}

func TestValidRequestID(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"a", "A1", "a-b_c.d:e/f+g=h", strings.Repeat("x", MaxRequestIDLength)} {
		if !validRequestID(ok) {
			t.Errorf("validRequestID(%.40q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"", // handled by the caller's non-empty check, but must not be adopted
		strings.Repeat("x", MaxRequestIDLength+1),
		"a b", "a\tb", "a\nb", "a\x00b", `a"b`, `a\b`, "a{b", "a;b", "a,b", "é",
	} {
		if bad == "" {
			continue // an absent header is chi's business, not this predicate's
		}
		if validRequestID(bad) {
			t.Errorf("validRequestID(%.40q) = true, want false", bad)
		}
	}
}
