package validate

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// runRule drives a single Validate-step middleware against a fixture body and
// returns whatever response (if any) the middleware set on ctx. nextCalled
// reports whether the rule allowed the request through.
func runRule(t *testing.T, mw maniflex.MiddlewareFunc, body map[string]any) (resp *maniflex.APIResponse, nextCalled bool) {
	t.Helper()
	ctx := &maniflex.ServerContext{}
	if body != nil {
		ctx.ParsedBody = maniflex.NewRequestBody(body)
	}
	err := mw(ctx, func() error {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}
	return ctx.Response, nextCalled
}

func TestNumericPrecision_ScaleExceeded(t *testing.T) {
	mw := NumericPrecision("amount", 19, 2)
	resp, called := runRule(t, mw, map[string]any{"amount": "10.123"})
	if called {
		t.Fatalf("next() was called for 3-decimal value under scale=2 limit")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
	if resp.Error == nil || resp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %+v", resp.Error)
	}
}

func TestNumericPrecision_ScaleAtLimit(t *testing.T) {
	mw := NumericPrecision("amount", 19, 2)
	_, called := runRule(t, mw, map[string]any{"amount": "10.12"})
	if !called {
		t.Fatalf("next() not called for value at scale=2 limit")
	}
}

func TestNumericPrecision_PrecisionExceeded(t *testing.T) {
	mw := NumericPrecision("amount", 5, 0) // 5 total digits, no scale limit
	resp, called := runRule(t, mw, map[string]any{"amount": "1234567"})
	if called {
		t.Fatalf("next() was called for 7-digit value under precision=5 limit")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
	wantSubstr := "5 total digit"
	if resp.Error == nil || !containsDetail(resp, wantSubstr) {
		t.Errorf("expected detail mentioning %q, got %+v", wantSubstr, resp.Error)
	}
}

func TestNumericPrecision_PrecisionCountsBothSides(t *testing.T) {
	// 4 integer digits + 2 fractional digits = 6 significant digits.
	mw := NumericPrecision("amount", 5, 4)
	resp, called := runRule(t, mw, map[string]any{"amount": "1234.56"})
	if called {
		t.Fatalf("next() was called for 6-significant-digit value under precision=5")
	}
	if resp == nil {
		t.Fatalf("expected validation response")
	}
}

func TestNumericPrecision_AcceptsJSONNumber(t *testing.T) {
	// Real requests deliver numbers as json.Number after Unmarshal with
	// UseNumber, or as float64 otherwise. Cover both paths through the
	// numericString switch.
	mw := NumericPrecision("amount", 19, 4)
	for name, body := range map[string]map[string]any{
		"float64":         {"amount": 12.3456},
		"int":             {"amount": int(1)},
		"int64":           {"amount": int64(99999)},
		"numeric string":  {"amount": "1234.5678"},
		"json.Number int": mustJSONBody(t, `{"amount": 42}`),
		"json.Number flt": mustJSONBody(t, `{"amount": 1.23}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, called := runRule(t, mw, body)
			if !called {
				t.Errorf("next() not called for %v", body)
			}
		})
	}
}

func TestNumericPrecision_RejectsScientificNotation(t *testing.T) {
	// Financial values should be supplied in plain form. 1e3 is a valid
	// float but its precision is ambiguous, so we reject it.
	mw := NumericPrecision("amount", 19, 2)
	resp, called := runRule(t, mw, map[string]any{"amount": "1e3"})
	if called {
		t.Fatalf("next() was called for scientific-notation value")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for scientific notation, got %+v", resp)
	}
}

func TestNumericPrecision_SkipsAbsentField(t *testing.T) {
	mw := NumericPrecision("amount", 19, 4)
	_, called := runRule(t, mw, map[string]any{"other": "x"})
	if !called {
		t.Errorf("next() not called when field is absent (rule should defer to `required`)")
	}
}

func TestNumericPrecision_SkipsNilField(t *testing.T) {
	mw := NumericPrecision("amount", 19, 4)
	_, called := runRule(t, mw, map[string]any{"amount": nil})
	if !called {
		t.Errorf("next() not called when field is explicit null")
	}
}

func TestNumericPrecision_SkipsNonNumericString(t *testing.T) {
	// Non-numeric strings are out of scope — they typically indicate a type
	// mismatch caught elsewhere (cast/parse). Don't double-fail here.
	mw := NumericPrecision("amount", 19, 4)
	_, called := runRule(t, mw, map[string]any{"amount": "hello"})
	if !called {
		t.Errorf("next() not called for non-numeric string")
	}
}

func TestNumericPrecision_PrecisionDisabledByZero(t *testing.T) {
	mw := NumericPrecision("amount", 0, 2) // no precision cap, scale=2
	_, called := runRule(t, mw, map[string]any{"amount": "999999999.99"})
	if !called {
		t.Errorf("next() not called when precision=0 (disabled)")
	}
}

func TestNumericPrecision_ScaleDisabledByZero(t *testing.T) {
	mw := NumericPrecision("amount", 5, 0) // precision=5, no scale cap
	_, called := runRule(t, mw, map[string]any{"amount": "1.2345"}) // 5 sig digits
	if !called {
		t.Errorf("next() not called for 5-digit value when scale=0 (disabled)")
	}
}

func TestNumericPrecision_HandlesNegativeAndPositiveSigns(t *testing.T) {
	mw := NumericPrecision("amount", 5, 2)
	for _, in := range []string{"-123.45", "+123.45"} {
		_, called := runRule(t, mw, map[string]any{"amount": in})
		if !called {
			t.Errorf("next() not called for %q (sign should not count toward precision)", in)
		}
	}
}

func TestNumericPrecision_LeadingZerosDoNotCount(t *testing.T) {
	mw := NumericPrecision("amount", 3, 2)
	_, called := runRule(t, mw, map[string]any{"amount": "0001.23"})
	if !called {
		t.Errorf("next() not called for value with leading zeros — they should be stripped")
	}
}

func TestNumericPrecision_TrailingFractionZerosDoNotCount(t *testing.T) {
	// 1.2300 has 3 significant digits, not 5.
	mw := NumericPrecision("amount", 3, 4)
	_, called := runRule(t, mw, map[string]any{"amount": "1.2300"})
	if !called {
		t.Errorf("next() not called for value with trailing fractional zeros")
	}
}

func TestNumericPrecision_ZeroBoundary(t *testing.T) {
	mw := NumericPrecision("amount", 1, 0)
	_, called := runRule(t, mw, map[string]any{"amount": "0"})
	if !called {
		t.Errorf("next() not called for plain 0 (should be a single significant digit)")
	}
}

func mustJSONBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(stringReader(raw))
	dec.UseNumber()
	body := map[string]any{}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return body
}

type stringReader string

func (s stringReader) Read(p []byte) (int, error) {
	if len(s) == 0 {
		return 0, errEOF
	}
	n := copy(p, s)
	return n, nil
}

var errEOF = errReader("EOF")

type errReader string

func (e errReader) Error() string { return string(e) }

func containsDetail(resp *maniflex.APIResponse, substr string) bool {
	if resp == nil || resp.Error == nil {
		return false
	}
	details, ok := resp.Error.Details.([]map[string]string)
	if !ok {
		return false
	}
	for _, d := range details {
		if msg, ok := d["message"]; ok && strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}
