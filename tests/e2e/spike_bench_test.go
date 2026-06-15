package e2e

// Phase-0 GATE benchmark (T0.3). Measures the per-row allocation cost of the
// struct-scan path against the current map path, head-to-head, under whichever
// driver lane is active. Run:
//
//	go test ./tests/e2e/... -run='^$' -bench=Spike -benchmem
//	MANIFLEX_TEST_DB=postgres go test ./tests/e2e/... -run='^$' -bench=Spike -benchmem -p 1
//
// Method: seed N rows, then capture the *already-scanned driver values* once
// (outside the timer). The timed loop converts those captured values — so the
// numbers reflect the framework's conversion cost, not the driver's row
// production (which is identical for both paths and present today regardless).
//
// Reference (authoritative, DB-free, core module serialize_bench_test.go):
//
//	BenchmarkSerialize_ToJSONMap_SingleRow   16 allocs/op   (the map serialize step)
//
// The map read path pays that 16/row to turn a scanned map into the response
// map; the struct path scans straight into *T and marshals it directly, paying
// 0 for a separate serialize step. So the comparison below (struct scan vs map
// scan) plus that fixed 16 is the full 16→≤4 picture the gate decides on.

import (
	"context"
	"testing"

	"maniflex"
	"maniflex/tests/spike"
	"maniflex/tests/e2e/testutil"
)

// spikeBenchModel mirrors the core benchWideModel: ~20 mixed-scalar columns plus
// BaseModel's id/created_at/updated_at, so the numbers line up with the
// published 16-alloc map baseline.
type spikeBenchModel struct {
	maniflex.BaseModel
	Name       string  `json:"name"`
	Email      string  `json:"email"`
	Age        int     `json:"age"`
	Score      float64 `json:"score"`
	Active     bool    `json:"active"`
	Bio        string  `json:"bio"`
	Status     string  `json:"status"`
	Department string  `json:"department"`
	Phone      string  `json:"phone"`
	Address    string  `json:"address"`
	City       string  `json:"city"`
	Country    string  `json:"country"`
	PostalCode string  `json:"postal_code"`
	LoginCount int64   `json:"login_count"`
	Rating     float32 `json:"rating"`
	Verified   bool    `json:"verified"`
	UserID     string  `json:"user_id"`
}

// spikeBenchSink defeats dead-code elimination.
var spikeBenchSink any

// captureRows seeds n rows and returns the column list plus the raw driver
// values for each row, exactly as *sql.Rows.Scan delivers them on this lane.
func captureRows(b *testing.B, n int) (*maniflex.ModelMeta, maniflex.DriverType, []string, [][]any) {
	b.Helper()
	srv := testutil.NewServer(b, testutil.Options{Models: []any{spikeBenchModel{}}})
	adapter := srv.ManiflexServer().DB()
	meta, ok := srv.ManiflexServer().Registry().Get("spikeBenchModel")
	if !ok {
		b.Fatal("spikeBenchModel not registered")
	}
	d := adapter.(interface{ DriverType() maniflex.DriverType }).DriverType()
	ctx := context.Background()

	for i := 0; i < n; i++ {
		m := &spikeBenchModel{
			Name: "Jane Q. Researcher", Email: "jane@hospital.example",
			Age: 34, Score: 87.5, Active: true,
			Bio: "Senior clinical data analyst.", Status: "active",
			Department: "cardiology", Phone: "+1-555-0142",
			Address: "1200 Medical Center Dr", City: "Springfield",
			Country: "US", PostalCode: "62704", LoginCount: int64(i),
			Rating: 4.5, Verified: false, UserID: "auth-user-9001",
		}
		q, args := spike.BuildInsert(meta, m, d)
		if _, err := adapter.Raw(ctx, q, args...).RowsAffected(); err != nil {
			b.Fatalf("seed row %d: %v", i, err)
		}
	}

	rows, err := adapter.Raw(ctx, "SELECT * FROM "+meta.TableName).Rows()
	if err != nil {
		b.Fatalf("select: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var raw [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			b.Fatalf("scan capture: %v", err)
		}
		raw = append(raw, vals)
	}
	return meta, d, cols, raw
}

func benchStructScan(b *testing.B, n int) {
	meta, drv, cols, raw := captureRows(b, n)
	sc := spike.NewStructScanner(meta, cols, drv)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]any, len(raw))
		for j, r := range raw {
			v, err := sc.ScanValues(r)
			if err != nil {
				b.Fatalf("ScanValues: %v", err)
			}
			out[j] = v
		}
		spikeBenchSink = out
	}
}

// benchMapScan reproduces db/sqlcore.scanRows' inner map build (the map path's
// scan step). The separate 16-alloc toJSONMap serialize step is NOT included
// here — see the reference note at the top of the file.
func benchMapScan(b *testing.B, n int) {
	_, _, cols, raw := captureRows(b, n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]any, len(raw))
		for j, r := range raw {
			m := make(map[string]any, len(cols))
			for k, c := range cols {
				v := r[k]
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				m[c] = v
			}
			out[j] = m
		}
		spikeBenchSink = out
	}
}

func BenchmarkSpike_StructScan_SingleRow(b *testing.B) { benchStructScan(b, 1) }
func BenchmarkSpike_StructScan_List100(b *testing.B)   { benchStructScan(b, 100) }
func BenchmarkSpike_StructScan_List1000(b *testing.B)  { benchStructScan(b, 1000) }

func BenchmarkSpike_MapScan_SingleRow(b *testing.B) { benchMapScan(b, 1) }
func BenchmarkSpike_MapScan_List100(b *testing.B)   { benchMapScan(b, 100) }
func BenchmarkSpike_MapScan_List1000(b *testing.B)  { benchMapScan(b, 1000) }
