package maniflex

// serialize_bench_test.go — micro-benchmarks for the per-row serialization hot
// path that the Response step runs on every read/list response.
//
// Unlike docs/perf/benchmark.mjs (a black-box end-to-end HTTP+SQLite load test),
// these isolate the framework's intrinsic CPU/allocation cost: no DB, no HTTP,
// no JSON encoder — just the reflection-driven map<->map conversion that scales
// with rows × fields. This is the path flagged in
// docs/plans/impl-plans/adoption_review.md §4.
//
// Run:
//
//	go test -bench=Serialize -benchmem -run='^$' -benchtime=2s .
//	go test -bench=Serialize -benchmem -run='^$' -cpuprofile=cpu.out .   # then: go tool pprof cpu.out
//
// Read the result as: ns/op = CPU per row; B/op + allocs/op = GC pressure per
// row. Multiply by expected page size (e.g. ×100 rows) to estimate the
// framework's floor under a list response before DB/HTTP costs are added.

import (
	"fmt"
	"reflect"
	"testing"
)

// benchWideModel mirrors a realistic "wide" business row (HR/EHR/ERP style):
// ~20 columns of mixed scalar types, one hidden and one write-only field that
// the response path must strip, and a convention FK. BaseModel adds id /
// created_at / updated_at.
type benchWideModel struct {
	BaseModel
	Name       string
	Email      string
	Age        int
	Score      float64
	Active     bool
	Bio        string
	Status     string
	Department string
	Phone      string
	Address    string
	City       string
	Country    string
	PostalCode string
	LoginCount int64
	Rating     float32
	Verified   bool
	Secret     string `mfx:"hidden"`    // must be stripped from every response
	Token      string `mfx:"writeonly"` // must be stripped from every response
	UserID     string // convention FK → relation "user" (not populated here)
}

// benchLocaleModel exercises the locale-resolution branch of toJSONMap, which
// is one of the per-row steps that accumulated over the feature batches (see
// the list-throughput drift noted in the adoption review).
type benchLocaleModel struct {
	BaseModel
	Name maniflex_localeString `mfx:"locale"`
	Code string
}

// maniflex_localeString is LocaleString under a local alias so the struct tag
// resolver treats the field as a locale column without importing anything new.
type maniflex_localeString = LocaleString

// buildBenchModel scans a model once (as registration does) and returns the
// metadata plus a synthetic DB row shaped the way the SQLite adapter's
// scanRows would deliver it: integers as int64, booleans as int64 0/1,
// timestamps and ids as strings (SQLite stores TIMESTAMP as TEXT).
func buildBenchWide(b testing.TB) (*ModelMeta, map[string]any) {
	b.Helper()
	meta, err := ScanModel(benchWideModel{}, ModelConfig{})
	if err != nil {
		b.Fatalf("ScanModel: %v", err)
	}
	row := map[string]any{
		"id":          "550e8400-e29b-41d4-a716-446655440000",
		"created_at":  "2026-05-29T10:00:00Z",
		"updated_at":  "2026-05-29T10:05:00Z",
		"name":        "Jane Q. Researcher",
		"email":       "jane.researcher@hospital.example",
		"age":         int64(34),   // SQLite returns INTEGER columns as int64
		"score":       87.5,        // REAL as float64
		"active":      int64(1),    // bool stored as 0/1 → exercises cast()
		"bio":         "Senior clinical data analyst, cardiology department.",
		"status":      "active",
		"department":  "cardiology",
		"phone":       "+1-555-0142",
		"address":     "1200 Medical Center Dr",
		"city":        "Springfield",
		"country":     "US",
		"postal_code": "62704",
		"login_count": int64(2471),
		"rating":      4.5,
		"verified":    int64(0),
		"secret":      "tope-secret-value-that-must-not-leak",
		"token":       "write-only-token-that-must-not-leak",
		"user_id":     "auth-user-9001",
	}
	return meta, row
}

// sink prevents the compiler from optimising the benchmarked work away.
var benchSink any

// buildBenchWideRecord returns a field-populated *benchWideModel equivalent to
// the buildBenchWide row, with every column marked present — i.e. the shape
// scanStruct delivers to the typed response serializer (marshalRecord).
func buildBenchWideRecord(tb testing.TB) (*ModelMeta, any) {
	tb.Helper()
	meta, row := buildBenchWide(tb)
	w := &benchWideModel{
		Name: "Jane Q. Researcher", Email: "jane.researcher@hospital.example",
		Age: 34, Score: 87.5, Active: true,
		Bio: "Senior clinical data analyst, cardiology department.", Status: "active",
		Department: "cardiology", Phone: "+1-555-0142", Address: "1200 Medical Center Dr",
		City: "Springfield", Country: "US", PostalCode: "62704", LoginCount: 2471,
		Rating: 4.5, Verified: false, Secret: "s", Token: "t", UserID: "auth-user-9001",
	}
	w.ID = "550e8400-e29b-41d4-a716-446655440000"
	cols := make([]string, 0, len(row))
	for k := range row {
		cols = append(cols, k)
	}
	SetPresentColumns(w, cols)
	return meta, w
}

func BenchmarkSerialize_ToJSONMap_SingleRow(b *testing.B) {
	s := newDefaultSteps(nil, nil)
	meta, row := buildBenchWide(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = s.toJSONMap(row, meta, nil)
	}
}

// marshalRecord is the typed-path response serializer (reads struct fields).
func BenchmarkSerialize_MarshalRecord_SingleRow(b *testing.B) {
	s := newDefaultSteps(nil, nil)
	meta, rec := buildBenchWideRecord(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = s.marshalRecord(meta, rec, nil)
	}
}

func BenchmarkSerialize_MarshalRecord_List1000(b *testing.B) {
	s := newDefaultSteps(nil, nil)
	meta, rec := buildBenchWideRecord(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]any, 1000)
		for j := range out {
			out[j] = s.marshalRecord(meta, rec, nil)
		}
		benchSink = out
	}
}

// The list benchmarks measure the actual Response-step OpList loop: N rows, one
// toJSONMap call each. ns/op is per *page*, so divide by the row count for
// per-row cost, or compare B/op across sizes to see allocation scaling.
func BenchmarkSerialize_ToJSONMap_List100(b *testing.B)  { benchList(b, 100) }
func BenchmarkSerialize_ToJSONMap_List1000(b *testing.B) { benchList(b, 1000) }

func benchList(b *testing.B, n int) {
	s := newDefaultSteps(nil, nil)
	meta, proto := buildBenchWide(b)

	// Distinct rows so map reuse / CPU-cache effects are realistic.
	rows := make([]map[string]any, n)
	for i := range rows {
		r := make(map[string]any, len(proto))
		for k, v := range proto {
			r[k] = v
		}
		r["id"] = fmt.Sprintf("row-%08d", i)
		r["login_count"] = int64(i)
		rows[i] = r
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := make([]any, n)
		for j, r := range rows {
			out[j] = s.toJSONMap(r, meta, nil)
		}
		benchSink = out
	}
}

// ToDBMap is the write-path counterpart: JSON-keyed body → DB-column map.
func BenchmarkSerialize_ToDBMap(b *testing.B) {
	meta, _ := buildBenchWide(b)
	body := map[string]any{
		"name": "Jane Q. Researcher", "email": "jane@hospital.example",
		"age": float64(34), "score": 87.5, "active": true,
		"bio": "analyst", "status": "active", "department": "cardiology",
		"phone": "+1-555-0142", "city": "Springfield", "country": "US",
		"login_count": float64(2471), "rating": 4.5, "verified": false,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = toDBMap(NewRequestBody(body), meta)
	}
}

// Cast isolates the per-cell type coercion. The string/bool/int variants cover
// the branches scanRows actually feeds it from SQLite (int64, 0/1 bools).
func BenchmarkSerialize_Cast(b *testing.B) {
	strT := reflect.TypeOf("")
	boolT := reflect.TypeOf(true)
	intT := reflect.TypeOf(int64(0))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = cast("a string value", strT)
		benchSink = cast(int64(1), boolT) // bool stored as int64 0/1
		benchSink = cast(int64(2471), intT)
	}
}

// Locale measures the per-row cost of LocaleString resolution (parse JSON +
// resolve chain) against a row carrying a two-locale value, with an explicit
// request locale set — the common ?locale=en read path.
func BenchmarkSerialize_ToJSONMap_Locale(b *testing.B) {
	s := newDefaultSteps(nil, nil)
	meta, err := ScanModel(benchLocaleModel{}, ModelConfig{})
	if err != nil {
		b.Fatalf("ScanModel: %v", err)
	}
	row := map[string]any{
		"id":         "550e8400-e29b-41d4-a716-446655440000",
		"created_at": "2026-05-29T10:00:00Z",
		"updated_at": "2026-05-29T10:00:00Z",
		"name":       `{"en":"Cardiology","ar":"أمراض القلب"}`,
		"code":       "CARD",
	}
	ctx := &ServerContext{Locale: "en", DefaultLocale: "en"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink = s.toJSONMap(row, meta, ctx)
	}
}
