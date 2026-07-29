package e2e

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func escapeFileKeySegments(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func TestStandaloneFileEncodedKeysRoundTripThroughGetAndDelete(t *testing.T) {
	t.Parallel()

	store := testutil.NewMemoryStorage()
	srv := fileServer(t, store)
	cases := []struct {
		name string
		key  string
		path string
	}{
		{
			name: "space",
			key:  "folders/report final.txt",
		},
		{
			name: "literal plus",
			key:  "folders/a+b.txt",
		},
		{
			name: "literal percent escape",
			key:  "folders/literal%2Fname.txt",
		},
		{
			name: "reserved characters",
			key:  "folders/report?#[]&=.txt",
		},
		{
			name: "unicode",
			key:  "مجلد/تقرير ✅.txt",
		},
		{
			name: "encoded slash separators",
			key:  "nested/deep/file.txt",
			path: url.PathEscape("nested/deep/file.txt"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte("payload for " + tc.name)
			testutil.PutObject(t, store, tc.key, "text/plain", body)

			path := tc.path
			if path == "" {
				path = escapeFileKeySegments(tc.key)
			}
			endpoint := "/files/" + path

			get := srv.GETRaw(endpoint)
			get.AssertStatus(http.StatusOK)
			if string(get.Body) != string(body) {
				t.Fatalf("GET body = %q, want %q", get.Body, body)
			}

			srv.DELETE(endpoint).AssertStatus(http.StatusNoContent)
			if store.HasKey(tc.key) {
				t.Fatalf("DELETE left canonical key %q in storage", tc.key)
			}
			srv.GETRaw(endpoint).AssertStatus(http.StatusNotFound)
		})
	}
}
