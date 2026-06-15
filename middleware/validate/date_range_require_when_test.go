package validate

import (
	"net/http"
	"testing"
)

// ── DateRange ─────────────────────────────────────────────────────────────────

func TestDateRange_PassesWhenBothAbsent(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{"other": "x"})
	if !called {
		t.Error("next() not called when both date fields are absent")
	}
}

func TestDateRange_PassesWhenStartAbsent(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{"end_date": "2026-05-10"})
	if !called {
		t.Error("next() not called when start field is absent")
	}
}

func TestDateRange_PassesWhenEndAbsent(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{"start_date": "2026-05-01"})
	if !called {
		t.Error("next() not called when end field is absent")
	}
}

func TestDateRange_PassesEndAfterStart(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{
		"start_date": "2026-05-01",
		"end_date":   "2026-05-10",
	})
	if !called {
		t.Error("next() not called for valid start < end range")
	}
}

func TestDateRange_PassesEndEqualsStart(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{
		"start_date": "2026-05-01",
		"end_date":   "2026-05-01",
	})
	if !called {
		t.Error("next() not called when start == end (equal dates are valid)")
	}
}

func TestDateRange_RejectsEndBeforeStart(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	resp, called := runRule(t, mw, map[string]any{
		"start_date": "2026-05-10",
		"end_date":   "2026-05-01",
	})
	if called {
		t.Error("next() must not be called when end < start")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
	if resp.Error == nil || resp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %+v", resp.Error)
	}
	if !containsDetail(resp, "end_date") {
		t.Errorf("error detail should mention end_date, got %+v", resp.Error)
	}
}

func TestDateRange_WorksWithRFC3339Timestamps(t *testing.T) {
	mw := DateRange("start", "end")
	resp, called := runRule(t, mw, map[string]any{
		"start": "2026-05-01T08:00:00Z",
		"end":   "2026-05-01T07:59:59Z", // one second before start
	})
	if called {
		t.Error("next() must not be called when RFC3339 end is before start")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
}

func TestDateRange_PassesUnparseableFields(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{
		"start_date": "not-a-date",
		"end_date":   "also-not-a-date",
	})
	if !called {
		t.Error("next() not called when field values are unparseable (should skip silently)")
	}
}

func TestDateRange_PassesNilFields(t *testing.T) {
	mw := DateRange("start_date", "end_date")
	_, called := runRule(t, mw, map[string]any{
		"start_date": nil,
		"end_date":   nil,
	})
	if !called {
		t.Error("next() not called when fields are nil")
	}
}

// ── RequireWhen ───────────────────────────────────────────────────────────────

func TestRequireWhen_PassesWhenConditionNotMet(t *testing.T) {
	mw := RequireWhen("rejection_reason", "status:eq:rejected")
	_, called := runRule(t, mw, map[string]any{"status": "approved"})
	if !called {
		t.Error("next() not called when condition is not met (field should not be required)")
	}
}

func TestRequireWhen_FailsWhenConditionMetAndFieldAbsent(t *testing.T) {
	mw := RequireWhen("rejection_reason", "status:eq:rejected")
	resp, called := runRule(t, mw, map[string]any{"status": "rejected"})
	if called {
		t.Error("next() must not be called when condition is met but field is absent")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
	if resp.Error == nil || resp.Error.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %+v", resp.Error)
	}
	if !containsDetail(resp, "rejection_reason") {
		t.Errorf("error detail should mention rejection_reason, got %+v", resp.Error)
	}
}

func TestRequireWhen_PassesWhenConditionMetAndFieldPresent(t *testing.T) {
	mw := RequireWhen("rejection_reason", "status:eq:rejected")
	_, called := runRule(t, mw, map[string]any{
		"status":           "rejected",
		"rejection_reason": "duplicate claim",
	})
	if !called {
		t.Error("next() not called when condition is met and field is present")
	}
}

func TestRequireWhen_FailsWhenFieldIsEmptyString(t *testing.T) {
	mw := RequireWhen("note", "type:eq:special")
	resp, called := runRule(t, mw, map[string]any{"type": "special", "note": ""})
	if called {
		t.Error("next() must not be called when condition is met but field is empty string")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
}

func TestRequireWhen_FailsWhenFieldIsNil(t *testing.T) {
	mw := RequireWhen("note", "type:eq:special")
	resp, called := runRule(t, mw, map[string]any{"type": "special", "note": nil})
	if called {
		t.Error("next() must not be called when condition is met but field is nil")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
}

func TestRequireWhen_AllConditionsMustHold(t *testing.T) {
	mw := RequireWhen("shipping_address", "order_type:eq:physical", "priority:gte:3")

	t.Run("only_first_condition_met", func(t *testing.T) {
		_, called := runRule(t, mw, map[string]any{
			"order_type": "physical",
			"priority":   "1",
		})
		if !called {
			t.Error("next() not called when only first condition is met")
		}
	})

	t.Run("only_second_condition_met", func(t *testing.T) {
		_, called := runRule(t, mw, map[string]any{
			"order_type": "digital",
			"priority":   "5",
		})
		if !called {
			t.Error("next() not called when only second condition is met")
		}
	})

	t.Run("both_met_field_absent", func(t *testing.T) {
		resp, called := runRule(t, mw, map[string]any{
			"order_type": "physical",
			"priority":   "5",
		})
		if called {
			t.Error("next() must not be called when both conditions are met and field is absent")
		}
		if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %+v", resp)
		}
	})
}

func TestRequireWhen_NeOp(t *testing.T) {
	mw := RequireWhen("reason", "status:ne:active")
	_, called := runRule(t, mw, map[string]any{"status": "active"}) // ne not satisfied
	if !called {
		t.Error("next() not called when ne condition is not satisfied")
	}
	resp, called2 := runRule(t, mw, map[string]any{"status": "inactive"}) // ne satisfied, field absent
	if called2 {
		t.Error("next() must not be called when ne condition is satisfied and field absent")
	}
	if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %+v", resp)
	}
}

func TestRequireWhen_NumericOperators(t *testing.T) {
	cases := []struct {
		name        string
		op          string
		bodyVal     string
		condVal     string
		shouldTrigger bool
	}{
		{"gt_triggers",   "gt",  "5", "3", true},
		{"gt_equal_skips","gt",  "3", "3", false},
		{"gte_equal",     "gte", "3", "3", true},
		{"lt_triggers",   "lt",  "1", "3", true},
		{"lt_equal_skips","lt",  "3", "3", false},
		{"lte_equal",     "lte", "3", "3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mw := RequireWhen("note", "score:"+tc.op+":"+tc.condVal)
			resp, called := runRule(t, mw, map[string]any{"score": tc.bodyVal})
			if tc.shouldTrigger {
				if called {
					t.Error("next() must not be called when condition is met and field is absent")
				}
				if resp == nil || resp.StatusCode != http.StatusUnprocessableEntity {
					t.Fatalf("expected 422, got %+v", resp)
				}
			} else {
				if !called {
					t.Error("next() not called when condition is not met")
				}
			}
		})
	}
}

func TestRequireWhen_ConditionFieldAbsent(t *testing.T) {
	mw := RequireWhen("reason", "status:eq:rejected")
	_, called := runRule(t, mw, map[string]any{"other": "x"})
	if !called {
		t.Error("next() not called when condition field is absent from body")
	}
}

func TestRequireWhen_PanicsOnInvalidConditionSyntax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid condition syntax, got none")
		}
	}()
	RequireWhen("field", "badcondition") // no colons → should panic
}
