package maniflex

import "testing"

func TestPresignURLOptions_ContentDisposition(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts PresignURLOptions
		want string
	}{
		{
			name: "zero value asks for no header",
			opts: PresignURLOptions{},
			want: "",
		},
		{
			name: "explicit value wins over Download and Filename",
			opts: PresignURLOptions{
				ResponseContentDisposition: `attachment; filename="override.pdf"`,
				Download:                   true,
				Filename:                   "ignored.pdf",
			},
			want: `attachment; filename="override.pdf"`,
		},
		{
			name: "download with an ASCII filename",
			opts: PresignURLOptions{Download: true, Filename: "report.pdf"},
			want: "attachment; filename=report.pdf",
		},
		{
			// The reason this method exists: a caller hand-rolling the header
			// would emit a raw UTF-8 filename= and lose the name in browsers
			// that follow the spec.
			name: "download with a non-ASCII filename uses the filename* form",
			opts: PresignURLOptions{Download: true, Filename: "résumé.pdf"},
			want: "attachment; filename*=utf-8''r%C3%A9sum%C3%A9.pdf",
		},
		{
			name: "download with no filename is the bare disposition",
			opts: PresignURLOptions{Download: true},
			want: "attachment",
		},
		{
			// A name without Download means "call it this", not "force a save".
			name: "filename alone names the object inline",
			opts: PresignURLOptions{Filename: "report.pdf"},
			want: "inline; filename=report.pdf",
		},
		{
			name: "a quote in the filename is quoted, not injected",
			opts: PresignURLOptions{Download: true, Filename: `in"quote.pdf`},
			want: `attachment; filename="in\"quote.pdf"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.ContentDisposition(); got != tc.want {
				t.Errorf("ContentDisposition() = %q, want %q", got, tc.want)
			}
		})
	}
}
