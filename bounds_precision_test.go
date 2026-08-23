package maniflex

import (
	"strings"
	"testing"
)

// The bound is rendered with %g, which switches to scientific notation as soon
// as that is the shorter form — so mfx:"max:1000000" told the client "must be
// <= 1e+06". That is a 422 body at a completely ordinary bound, and it reads as
// a framework defect rather than as the limit the model declared (audit O5).
func TestCheckFieldValueRendersBoundsAsWritten(t *testing.T) {
	cases := []struct {
		name  string
		bound float64
		val   any
		want  string
	}{
		{"a million", 1_000_000, 2_000_000.0, "1000000"},
		{"five million", 5_000_000, 9_000_000.0, "5000000"},
		{"small integer keeps its shape", 100, 200.0, "100"},
		// A fractional bound must still read as a fraction: fixing the exponent
		// must not turn 0.5 into 0 or 1.
		{"fraction", 0.5, 2.0, "0.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bound := tc.bound
			f := &FieldMeta{
				Name: "N",
				Tags: FieldTags{JSONName: "n", Max: &bound},
			}
			msg := checkFieldValue(f, tc.val)
			if msg == "" {
				t.Fatalf("value %v did not violate max %v", tc.val, tc.bound)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("message %q does not contain %q", msg, tc.want)
			}
			if strings.Contains(msg, "e+") || strings.Contains(msg, "E+") {
				t.Errorf("message uses scientific notation: %q", msg)
			}
		})
	}

	// The min side is formatted by the same call and drifts the same way.
	bound := 1_000_000.0
	f := &FieldMeta{Name: "N", Tags: FieldTags{JSONName: "n", Min: &bound}}
	if msg := checkFieldValue(f, 1.0); !strings.Contains(msg, "1000000") {
		t.Errorf("min message %q does not contain 1000000", msg)
	}
}

// ── Bounds that float64 cannot hold ──────────────────────────────────────────

type boundsHugeInt struct {
	BaseModel
	N int64 `json:"n" mfx:"max:9223372036854775807"`
}

type boundsHugeIntMin struct {
	BaseModel
	N int64 `json:"n" mfx:"min:-9223372036854775807"`
}

type boundsOrdinaryInt struct {
	BaseModel
	N int64 `json:"n" mfx:"max:1000000"`
}

type boundsHugeFloat struct {
	BaseModel
	N float64 `json:"n" mfx:"max:1e20"`
}

// A min:/max: tag is parsed with ParseFloat, so a bound past 2^53 is stored as
// whatever float64 rounds it to and then enforced as that. The rounding goes
// both ways: max:9007199254740995 becomes ...996, a ceiling looser than the one
// written, and max:9223372036854775807 becomes 9223372036854776000 — above
// MaxInt64, so the guard can never fire and the column is silently unbounded.
//
// Registration refuses it rather than enforcing a number nobody wrote. The
// limit is the whole exactly-representable range rather than the subset of
// larger integers float64 happens to hit, because "some values in this range
// work" is not a contract worth offering.
//
// It applies to integer-typed fields only. A float column's bound is inexact by
// the nature of the column, and a caller writing max:1e20 there has asked for a
// magnitude rather than an exact value.
func TestRegisterRejectsBoundsBeyondExactFloat64(t *testing.T) {
	t.Run("int64 max past 2^53 is refused", func(t *testing.T) {
		err := New(Config{DisableAutoMigrate: true}).Register(boundsHugeInt{})
		if err == nil {
			t.Fatal("registered a bound float64 cannot represent")
		}
		if !strings.Contains(err.Error(), "N") {
			t.Errorf("error does not name the field: %v", err)
		}
	})

	t.Run("int64 min past 2^53 is refused", func(t *testing.T) {
		if err := New(Config{DisableAutoMigrate: true}).Register(boundsHugeIntMin{}); err == nil {
			t.Fatal("registered a min bound float64 cannot represent")
		}
	})

	t.Run("an ordinary integer bound still registers", func(t *testing.T) {
		if err := New(Config{DisableAutoMigrate: true}).Register(boundsOrdinaryInt{}); err != nil {
			t.Fatalf("rejected a perfectly representable bound: %v", err)
		}
	})

	t.Run("a float column may carry a large bound", func(t *testing.T) {
		if err := New(Config{DisableAutoMigrate: true}).Register(boundsHugeFloat{}); err != nil {
			t.Fatalf("rejected a magnitude bound on a float column: %v", err)
		}
	})
}
