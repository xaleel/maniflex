package maniflex

// Side-by-side A/B for cast. Separate benchmark runs on this machine drift by 3x,
// so the only trustworthy comparison puts both implementations in one binary and
// one run. castViaReflect is the pre-PERF-4 implementation, kept here (test-only)
// purely as the baseline arm.
//
//	go test -run '^$' -bench BenchmarkCastAB -benchmem

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// castViaReflect is the original: it asks reflect for the dynamic kind of every
// value, per field per row. Kept verbatim as the baseline arm — including the
// nesting that splitting it up removed.
//
//nolint:gocognit // a frozen copy of the pre-fix implementation; not maintained
func castViaReflect(value any, _type reflect.Type) any {
	if value == nil {
		return nil
	}
	if _type.Kind() == reflect.String {
		if reflect.TypeOf(value).Kind() == reflect.String {
			return value.(string)
		}
		return fmt.Sprintf("%s", value)
	}
	if _type.Kind() == reflect.Bool {
		if reflect.TypeOf(value).Kind() == reflect.Bool {
			return value.(bool)
		}
		if reflect.TypeOf(value).Kind() == reflect.String {
			s := value.(string)
			if b, err := strconv.ParseBool(s); err == nil {
				return b
			}
			switch strings.ToLower(s) {
			case "", "no", "off", "n":
				return false
			}
			return true
		}
		if isNumberAndGreaterThanZero(value) {
			return true
		}
		return false
	}
	return value
}

func BenchmarkCastAB(b *testing.B) {
	strT := reflect.TypeOf("")
	boolT := reflect.TypeOf(true)
	intT := reflect.TypeOf(int64(0))

	// The cells scanRows actually feeds it from SQLite: a text column, a bool
	// stored as int64 0/1, and a plain integer.
	run := func(b *testing.B, f func(any, reflect.Type) any) {
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			benchSink = f("a string value", strT)
			benchSink = f(int64(1), boolT)
			benchSink = f(int64(2471), intT)
		}
	}

	b.Run("reflect", func(b *testing.B) { run(b, castViaReflect) })
	b.Run("typeswitch", func(b *testing.B) { run(b, cast) })
}
