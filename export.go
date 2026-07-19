package maniflex

import (
	"archive/zip"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ExportFormat selects the wire format the export endpoint produces.
type ExportFormat string

const (
	ExportFormatCSV  ExportFormat = "csv"
	ExportFormatXLSX ExportFormat = "xlsx"
)

// parseExportFormat resolves the ?format= query parameter. Defaults to CSV
// when absent; returns an error for any value other than csv/xlsx so the
// client gets a clear 400 instead of a surprise CSV.
func parseExportFormat(raw string) (ExportFormat, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "csv":
		return ExportFormatCSV, nil
	case "xlsx":
		return ExportFormatXLSX, nil
	}
	return "", fmt.Errorf("unsupported export format %q (use csv or xlsx)", raw)
}

// exportColumns returns the JSON field names that should appear in an export,
// in declaration order. Hidden, writeonly, and file fields are excluded
// (writeonly never appears in responses; file fields would dump opaque keys
// the recipient can't use).
//
// Fields declared through ctx.RedactResponseField are excluded too (audit
// MS-11). Those tags are static — the same for every caller — while a masking
// middleware decides per request, and an export used to consult only the tags:
// an app that hid salary from non-admins on the JSON path served it in full at
// /employees/export. The column is dropped from the header as well as the rows,
// so the export does not advertise a field it will not fill.
//
// ctx may be nil, which selects on the tags alone.
func exportColumns(model *ModelMeta, ctx *ServerContext) []FieldMeta {
	out := make([]FieldMeta, 0, len(model.Fields))
	for _, f := range model.Fields {
		if f.Tags.Hidden || f.Tags.WriteOnly {
			continue
		}
		if f.Tags.File {
			continue
		}
		if ctx != nil && ctx.IsFieldRedacted(f.Tags.JSONName) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// writeExportCSV writes `rows` as a CSV document with a header row to w.
// Values are stringified via fmt.Sprintf("%v", v); nil becomes empty string.
//
// rows is pulled one at a time rather than taken as a slice so that a row's map
// is garbage the moment it has been written, instead of the whole export living
// in memory at once.
func writeExportCSV(w io.Writer, fields []FieldMeta, rows iter.Seq[map[string]any]) error {
	cw := csv.NewWriter(w)
	header := make([]string, len(fields))
	for i, f := range fields {
		header[i] = f.Tags.JSONName
	}
	if err := cw.Write(header); err != nil {
		return err
	}
	rec := make([]string, len(fields))
	var werr error
	for row := range rows {
		for i, f := range fields {
			rec[i] = formatCell(row[f.Tags.JSONName])
		}
		if werr = cw.Write(rec); werr != nil {
			break
		}
	}
	if werr != nil {
		return werr
	}
	cw.Flush()
	return cw.Error()
}

func formatCell(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// writeExportXLSX writes a minimal-but-valid xlsx file: a ZIP container with
// the four XML members Excel needs to open the document. Cells are written
// as inline strings (`t="inlineStr"`) so there's no shared-string table to
// maintain — slightly larger files, much simpler writer.
func writeExportXLSX(w io.Writer, fields []FieldMeta, rows iter.Seq[map[string]any]) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	// [Content_Types].xml — tells Excel what each part of the zip is.
	if err := writeZipFile(zw, "[Content_Types].xml", xlsxContentTypes); err != nil {
		return err
	}
	// _rels/.rels — root relationship pointing at the workbook.
	if err := writeZipFile(zw, "_rels/.rels", xlsxRootRels); err != nil {
		return err
	}
	// xl/_rels/workbook.xml.rels — workbook → sheet relationship.
	if err := writeZipFile(zw, "xl/_rels/workbook.xml.rels", xlsxWorkbookRels); err != nil {
		return err
	}
	// xl/workbook.xml — declares one sheet named "Data".
	if err := writeZipFile(zw, "xl/workbook.xml", xlsxWorkbook); err != nil {
		return err
	}
	// xl/worksheets/sheet1.xml — the actual rows.
	sheet, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return err
	}
	return writeXLSXSheet(sheet, fields, rows)
}

func writeZipFile(zw *zip.Writer, name, body string) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, body)
	return err
}

func writeXLSXSheet(w io.Writer, fields []FieldMeta, rows iter.Seq[map[string]any]) error {
	// XML escape helper for inline strings.
	esc := func(s string) string {
		var sb strings.Builder
		_ = xml.EscapeText(&sb, []byte(s))
		return sb.String()
	}

	if _, err := io.WriteString(w, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`); err != nil {
		return err
	}

	// Header row (row 1).
	if _, err := io.WriteString(w, `<row r="1">`); err != nil {
		return err
	}
	for i, f := range fields {
		cell := fmt.Sprintf(`<c r="%s1" t="inlineStr"><is><t>%s</t></is></c>`,
			columnLetter(i), esc(f.Tags.JSONName))
		if _, err := io.WriteString(w, cell); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, `</row>`); err != nil {
		return err
	}

	// Data rows (start at row 2).
	rowNum := 1
	var werr error
	for row := range rows {
		rowNum++
		if werr = writeXLSXRow(w, fields, row, rowNum, esc); werr != nil {
			break
		}
	}
	if werr != nil {
		return werr
	}

	_, err := io.WriteString(w, `</sheetData></worksheet>`)
	return err
}

func writeXLSXRow(w io.Writer, fields []FieldMeta, row map[string]any, rowNum int,
	esc func(string) string,
) error {
	if _, err := fmt.Fprintf(w, `<row r="%d">`, rowNum); err != nil {
		return err
	}
	for ci, f := range fields {
		cell := fmt.Sprintf(`<c r="%s%d" t="inlineStr"><is><t>%s</t></is></c>`,
			columnLetter(ci), rowNum, esc(formatCell(row[f.Tags.JSONName])))
		if _, err := io.WriteString(w, cell); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `</row>`)
	return err
}

// columnLetter converts 0-based column index to A, B, …, Z, AA, AB, …
// xlsx uses 1-based letters; the input is 0-based for convenience.
func columnLetter(i int) string {
	out := ""
	i++
	for i > 0 {
		i--
		out = string(rune('A'+(i%26))) + out
		i /= 26
	}
	return out
}

// streamExport writes the export body to the supplied ResponseWriter. It
// sets Content-Type and Content-Disposition headers, calls WriteHeader(200)
// to lock them in before any body bytes flush (the csv.Writer buffers, so
// without an explicit WriteHeader we'd be at the mercy of the Flush
// timing and content-type sniffing), then writes the body.
func streamExport(w http.ResponseWriter, modelName string, format ExportFormat, fields []FieldMeta, rows iter.Seq[map[string]any]) error {
	fname := fmt.Sprintf("%s-%s", strings.ToLower(modelName), time.Now().UTC().Format("20060102-150405"))
	switch format {
	case ExportFormatCSV:
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, fname))
		w.WriteHeader(http.StatusOK)
		return writeExportCSV(w, fields, rows)
	case ExportFormatXLSX:
		w.Header().Set("Content-Type",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.xlsx"`, fname))
		w.WriteHeader(http.StatusOK)
		return writeExportXLSX(w, fields, rows)
	}
	return fmt.Errorf("unknown export format")
}

// XLSX boilerplate. These are the minimum-viable parts Excel needs to open a
// single-sheet workbook with inline strings; everything else (styles, shared
// strings, comments) is omitted intentionally.
const (
	xlsxContentTypes = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="xml" ContentType="application/xml"/>
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
</Types>`

	xlsxRootRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>`

	xlsxWorkbookRels = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`

	xlsxWorkbook = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets><sheet name="Data" sheetId="1" r:id="rId1"/></sheets>
</workbook>`
)
