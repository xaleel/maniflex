package maniflex

// MS-PERF-B — despite the "stream" naming, an export used to build a second
// full []map[string]any of the entire result set and hand it to the writer,
// so two copies were live at once for the length of the write. The writers now
// pull one row at a time, which is only worth anything if they hold nothing:
// these tests pin that, and that the output did not change in the process.

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"iter"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func streamFields(names ...string) []FieldMeta {
	out := make([]FieldMeta, len(names))
	for i, n := range names {
		out[i] = FieldMeta{
			Name: n, Type: reflect.TypeOf(""),
			Tags: FieldTags{DBName: n, JSONName: n},
		}
	}
	return out
}

// seqOf makes an iter.Seq over a fixed slice, for the output-shape tests.
func seqOf(rows []map[string]any) iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// generatedRows yields n rows without ever holding more than one, so whatever a
// writer retains is the writer's own doing and not the fixture's.
func generatedRows(fields []FieldMeta, n int, counter *int) iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		for i := range n {
			m := make(map[string]any, len(fields))
			for _, f := range fields {
				m[f.Tags.JSONName] = fmt.Sprintf("row-%d-%s-padding-padding-padding", i, f.Tags.JSONName)
			}
			if counter != nil {
				*counter++
			}
			if !yield(m) {
				return
			}
		}
	}
}

// peakHeapDuring samples the heap while f runs and returns the highest reading.
// Sampling in a spin loop is crude, but the property under test is a 10x
// difference in growth, not a precise figure.
func peakHeapDuring(f func()) uint64 {
	runtime.GC()
	var peak uint64
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
		}
	}()
	f()
	close(stop)
	<-finished // also the happens-before that makes reading peak safe
	return peak
}

// The point of the change: what a writer holds must not grow with the row count.
// Ten times the rows must not cost anything like ten times the heap.
func TestExportWriters_HoldNothingPerRow(t *testing.T) {
	fields := streamFields("a", "b", "c", "d", "e", "f", "g", "h")

	writers := map[string]func(io.Writer, []FieldMeta, iter.Seq[map[string]any]) error{
		"csv":  writeExportCSV,
		"xlsx": writeExportXLSX,
	}

	for name, write := range writers {
		small := peakHeapDuring(func() {
			if err := write(io.Discard, fields, generatedRows(fields, 10_000, nil)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		})
		large := peakHeapDuring(func() {
			if err := write(io.Discard, fields, generatedRows(fields, 100_000, nil)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		})

		// 10x the rows. A writer that accumulated would show something near 10x;
		// 3x is loose enough to absorb GC pacing on a noisy machine and still
		// catch accumulation.
		if large > small*3 {
			t.Errorf("%s: peak heap went %d KB -> %d KB for 10x the rows — the writer is "+
				"accumulating rows rather than writing them through",
				name, small/1024, large/1024)
		}
		t.Logf("%s: 10k rows %d KB, 100k rows %d KB", name, small/1024, large/1024)
	}
}

// A row must be pulled only when the writer is ready for it — if the writer
// drained the sequence up front it would be holding the whole export again,
// just one level down.
func TestExportWriters_PullRowsLazily(t *testing.T) {
	fields := streamFields("a", "b")

	for _, name := range []string{"csv", "xlsx"} {
		produced := 0
		var seen []int

		// A writer that records how many rows had been produced by the time each
		// chunk of output was written. If rows were drained first, every reading
		// would already be at the total.
		w := writerFunc(func(p []byte) (int, error) {
			seen = append(seen, produced)
			return len(p), nil
		})

		// Enough rows to overflow the writers' own buffering several times over.
		// csv.Writer wraps a 4KB bufio and the zip writer deflates into one of its
		// own, so a handful of small rows all reach the underlying writer at Flush
		// no matter how they were sourced — which says nothing either way.
		const n = 5_000
		var err error
		if name == "csv" {
			err = writeExportCSV(w, fields, generatedRows(fields, n, &produced))
		} else {
			err = writeExportXLSX(w, fields, generatedRows(fields, n, &produced))
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if produced != n {
			t.Fatalf("%s: produced %d rows, want %d", name, produced, n)
		}

		// Some write must have happened while fewer than all rows existed.
		interleaved := false
		for _, s := range seen {
			if s < n {
				interleaved = true
				break
			}
		}
		if !interleaved {
			t.Errorf("%s: every one of the %d writes happened only after all %d rows had "+
				"been produced — the writer drains the sequence up front, so the whole "+
				"export is materialised again just one level down", name, len(seen), n)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Switching the writers from a slice to a sequence must not have moved a byte of
// the output. CSV: header then rows, in field order.
func TestExportCSV_OutputUnchanged(t *testing.T) {
	fields := streamFields("name", "email")
	rows := []map[string]any{
		{"name": "Ada", "email": "ada@example.com"},
		{"name": "Grace, Rear Admiral", "email": "grace@example.com"}, // comma must quote
		{"name": nil, "email": ""},                                    // nil becomes empty
	}

	var buf bytes.Buffer
	if err := writeExportCSV(&buf, fields, seqOf(rows)); err != nil {
		t.Fatal(err)
	}

	got, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	want := [][]string{
		{"name", "email"},
		{"Ada", "ada@example.com"},
		{"Grace, Rear Admiral", "grace@example.com"},
		{"", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d CSV records, want %d: %q", len(got), len(want), buf.String())
	}
	for i := range want {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Errorf("cell [%d][%d] = %q, want %q", i, j, got[i][j], want[i][j])
			}
		}
	}
}

// XLSX: the row numbering is now driven by a counter rather than the slice
// index, which is exactly the kind of thing an off-by-one lands in. Row 1 is the
// header, so data starts at r="2" and must not skip or repeat.
func TestExportXLSX_RowNumbering(t *testing.T) {
	fields := streamFields("name")
	rows := []map[string]any{{"name": "first"}, {"name": "second"}, {"name": "third"}}

	var buf bytes.Buffer
	if err := writeExportXLSX(&buf, fields, seqOf(rows)); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("output is not a valid zip: %v", err)
	}
	var sheet string
	for _, f := range zr.File {
		if f.Name != "xl/worksheets/sheet1.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		sheet = string(b)
	}
	if sheet == "" {
		t.Fatal("no sheet1.xml in the workbook")
	}

	for _, want := range []string{`<row r="1">`, `<row r="2">`, `<row r="3">`, `<row r="4">`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet is missing %s — row numbering is off:\n%s", want, sheet)
		}
	}
	if strings.Contains(sheet, `<row r="5">`) {
		t.Errorf("sheet has a row 5 for 3 data rows + 1 header:\n%s", sheet)
	}
	for i, want := range []string{`<t>name</t>`, `<t>first</t>`, `<t>second</t>`, `<t>third</t>`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet is missing cell %d (%s):\n%s", i, want, sheet)
		}
	}
}

// An empty result must still produce a header-only document, not a broken one.
func TestExportWriters_NoRows(t *testing.T) {
	fields := streamFields("name")
	empty := seqOf(nil)

	var csvBuf bytes.Buffer
	if err := writeExportCSV(&csvBuf, fields, empty); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(csvBuf.String()); got != "name" {
		t.Errorf("empty CSV export = %q, want just the header", got)
	}

	var xlsxBuf bytes.Buffer
	if err := writeExportXLSX(&xlsxBuf, fields, empty); err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(xlsxBuf.Bytes()), int64(xlsxBuf.Len())); err != nil {
		t.Errorf("empty XLSX export is not a valid zip: %v", err)
	}
}
