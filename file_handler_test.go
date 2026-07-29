package maniflex

import (
	"context"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestSanitizeFilename pins the stricter rules introduced for roadmap §11C.2.
// Pre-fix the function only stripped `/`, `\`, and NUL — so `..`, control
// characters, and unbounded length all survived.
func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty maps to placeholder", "", "unnamed"},
		{"dot only maps to placeholder", ".", "unnamed"},
		{"double dot only maps to placeholder", "..", "unnamed"},
		{"leading dots are stripped", "...hidden.txt", "hidden.txt"},
		{"forward slash flattened", "a/b/c.txt", "a_b_c.txt"},
		{"backslash flattened", `a\b\c.txt`, "a_b_c.txt"},
		{"CR LF flattened", "a\r\nb.txt", "a__b.txt"},
		{"NUL flattened", "a\x00b.txt", "a_b.txt"},
		{"unicode flattened", "naïve.txt", "na_ve.txt"},
		{"safe chars survive", "Report_2026-05-25.pdf", "Report_2026-05-25.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeFilename(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeFilename(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeFilename_TruncatesLongNames(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := sanitizeFilename(long)
	if len(got) != maxFilenameLen {
		t.Errorf("got length %d, want %d", len(got), maxFilenameLen)
	}
}

func TestDecodeWildcardFileKeyDecodesExactlyOnce(t *testing.T) {
	t.Parallel()

	request := func(path, rawPath, wildcard string) *http.Request {
		req := httptest.NewRequest("GET", "/files/test", nil)
		req.URL.Path = path
		req.URL.RawPath = rawPath
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("*", wildcard)
		return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}

	for _, tc := range []struct {
		name     string
		path     string
		rawPath  string
		wildcard string
		want     string
		wantErr  bool
	}{
		{
			name:     "ordinary wildcard is already decoded",
			path:     "/files/report final.txt",
			wildcard: "report final.txt",
			want:     "report final.txt",
		},
		{
			name:     "literal percent escape is not decoded twice",
			path:     "/files/literal%2Fname.txt",
			wildcard: "literal%2Fname.txt",
			want:     "literal%2Fname.txt",
		},
		{
			name:     "raw encoded slash is decoded",
			path:     "/files/folder/name.txt",
			rawPath:  "/files/folder%2Fname.txt",
			wildcard: "folder%2Fname.txt",
			want:     "folder/name.txt",
		},
		{
			name:     "plus uses path not query semantics",
			path:     "/files/a+b.txt",
			rawPath:  "/files/a+b.txt",
			wildcard: "a+b.txt",
			want:     "a+b.txt",
		},
		{
			name:     "malformed raw escape is rejected",
			path:     "/files/bad",
			rawPath:  "/files/bad%zz",
			wildcard: "bad%zz",
			wantErr:  true,
		},
		{
			name:    "missing wildcard is rejected",
			path:    "/files/",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeWildcardFileKey(request(tc.path, tc.rawPath, tc.wildcard))
			if (err != nil) != tc.wantErr {
				t.Fatalf("decode error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("decoded key = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetFileHeaders_FormatsUntrustedFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		filename     string
		wantExtended bool
	}{
		{
			name:     "parameter syntax is quoted",
			filename: `report"; x=evil.txt`,
		},
		{
			name:         "control bytes are encoded",
			filename:     "report.pdf\r\nX-Injected: yes",
			wantExtended: true,
		},
		{
			name:         "UTF-8 uses extended parameter",
			filename:     "\u062a\u0642\u0631\u064a\u0631 2026.pdf",
			wantExtended: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			setFileHeaders(rec, FileMeta{
				ContentType: "application/pdf",
				Filename:    tc.filename,
			})

			header := rec.Header().Get("Content-Disposition")
			if strings.ContainsAny(header, "\r\n") {
				t.Fatalf("Content-Disposition contains a raw line break: %q", header)
			}
			mediaType, params, err := mime.ParseMediaType(header)
			if err != nil {
				t.Fatalf("parse Content-Disposition %q: %v", header, err)
			}
			if mediaType != "inline" {
				t.Errorf("media type = %q, want inline", mediaType)
			}
			if got := params["filename"]; got != tc.filename {
				t.Errorf("filename parameter = %q, want %q", got, tc.filename)
			}
			if got := strings.Contains(strings.ToLower(header), "filename*=utf-8''"); got != tc.wantExtended {
				t.Errorf("extended filename parameter present = %v, want %v; header: %q",
					got, tc.wantExtended, header)
			}
		})
	}
}

func TestSanitizeFilename_StripsLeadingDotsFromHostileNames(t *testing.T) {
	// A real-world attack surface: `..` and friends embedded at the start of
	// a filename can become hidden files in storage. Strip them.
	cases := []string{
		"....htaccess",
		". .start",
		"..\u200b.start",
	}
	for _, in := range cases {
		got := sanitizeFilename(in)
		if strings.HasPrefix(got, ".") {
			t.Errorf("sanitizeFilename(%q) = %q, must not start with '.'", in, got)
		}
	}
}
