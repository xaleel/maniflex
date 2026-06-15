package e2e

// 8.3 — auto-generated CSV/XLSX export endpoint. The route is mounted only
// when ModelConfig.ExportEnabled; it reuses the standard list query (filter,
// sort) and streams CSV (default) or XLSX (?format=xlsx) directly.

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// exportServer wires testutil.ExportableRow with ExportEnabled.
func exportServer(t *testing.T, maxRows int) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.ExportableRow{},
			maniflex.ModelConfig{ExportEnabled: true, MaxExportRows: maxRows},
		},
	})
}

func seedRows(t *testing.T, srv *testutil.Server, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		srv.POST("/exportable_rows", map[string]any{
			"name":   "user-" + itoa(i),
			"email":  "u" + itoa(i) + "@example.com",
			"secret": "s",
			"notes":  "private",
		}).AssertStatus(http.StatusCreated)
	}
}

func itoa(i int) string {
	// Tiny helper to avoid importing strconv where one call suffices.
	switch {
	case i < 10:
		return string(rune('0' + i))
	case i < 100:
		return string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return ""
}

func TestExport_CSVDefault(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 0)
	seedRows(t, srv, 3)

	resp := srv.GETRaw("/exportable_rows/export")
	resp.AssertStatus(http.StatusOK)

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type: got %q, want text/csv*", ct)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Errorf("Content-Disposition: got %q, want attachment", resp.Header.Get("Content-Disposition"))
	}

	rows, err := csv.NewReader(bytes.NewReader(resp.Body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 4 { // 1 header + 3 data
		t.Errorf("rows: got %d, want 4 (header + 3 data)", len(rows))
	}
	header := rows[0]
	// Hidden + writeonly columns must NOT appear.
	for _, col := range header {
		if col == "secret" || col == "notes" {
			t.Errorf("excluded column %q leaked into header", col)
		}
	}
	// Required columns must appear.
	expected := map[string]bool{"id": false, "name": false, "email": false}
	for _, col := range header {
		if _, ok := expected[col]; ok {
			expected[col] = true
		}
	}
	for k, present := range expected {
		if !present {
			t.Errorf("expected column %q missing from CSV header", k)
		}
	}
}

func TestExport_FilterAndSortApplied(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 0)
	// Seed three rows; one we filter against.
	for _, name := range []string{"alpha", "beta", "alpha"} {
		srv.POST("/exportable_rows", map[string]any{"name": name}).AssertStatus(http.StatusCreated)
	}

	resp := srv.GETRaw("/exportable_rows/export?filter=name:eq:alpha&sort=created_at:asc")
	resp.AssertStatus(http.StatusOK)
	rows, err := csv.NewReader(bytes.NewReader(resp.Body)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 3 { // header + 2 alphas
		t.Errorf("rows: got %d, want 3 (header + 2 alphas)", len(rows))
	}
	// Spot-check the name column actually says "alpha".
	nameCol := -1
	for i, h := range rows[0] {
		if h == "name" {
			nameCol = i
		}
	}
	if nameCol < 0 {
		t.Fatal("name column not found")
	}
	for _, r := range rows[1:] {
		if r[nameCol] != "alpha" {
			t.Errorf("filter not applied: row name = %q", r[nameCol])
		}
	}
}

func TestExport_XLSXFormat(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 0)
	seedRows(t, srv, 2)

	resp := srv.GETRaw("/exportable_rows/export?format=xlsx")
	resp.AssertStatus(http.StatusOK)
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "spreadsheetml.sheet") {
		t.Errorf("Content-Type: got %q, want xlsx MIME", ct)
	}

	// Validate it's actually a parseable zip with the expected workbook
	// members.
	zr, err := zip.NewReader(bytes.NewReader(resp.Body), int64(len(resp.Body)))
	if err != nil {
		t.Fatalf("zip parse: %v", err)
	}
	want := map[string]bool{
		"[Content_Types].xml":           false,
		"_rels/.rels":                   false,
		"xl/_rels/workbook.xml.rels":    false,
		"xl/workbook.xml":               false,
		"xl/worksheets/sheet1.xml":      false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
	}
	for k, present := range want {
		if !present {
			t.Errorf("xlsx is missing required part %q", k)
		}
	}

	// Spot-check the sheet contains both seeded names.
	sheet, _ := zr.Open("xl/worksheets/sheet1.xml")
	body, _ := io.ReadAll(sheet)
	for _, want := range []string{"user-0", "user-1"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("sheet1.xml missing %q", want)
		}
	}
}

func TestExport_UnsupportedFormatReturns400(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 0)
	resp := srv.GETRaw("/exportable_rows/export?format=pdf")
	if resp.Status != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.Status)
	}
}

func TestExport_RouteNotMountedWhenDisabled(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.ExportableRow{}}, // no ModelConfig → ExportEnabled false
	})
	resp := srv.GETRaw("/exportable_rows/export")
	// chi returns 404 for unmounted routes.
	if resp.Status != http.StatusNotFound {
		t.Errorf("export route should be absent when not enabled; got %d", resp.Status)
	}
}

func TestExport_MaxRowsEnforced(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 2) // cap at 2 rows
	seedRows(t, srv, 5)       // seed 5 — over the cap

	resp := srv.GETRaw("/exportable_rows/export")
	if resp.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("status: got %d, want 413", resp.Status)
	}
}

func TestExport_AtCapSucceeds(t *testing.T) {
	t.Parallel()
	srv := exportServer(t, 5)
	seedRows(t, srv, 5)

	resp := srv.GETRaw("/exportable_rows/export")
	if resp.Status != http.StatusOK {
		t.Errorf("status at cap: got %d, want 200", resp.Status)
	}
}
