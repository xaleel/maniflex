package maniflex

import "testing"

func TestParseLockWhen_ValidAndInvalid(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want LockCondition
	}{
		{"lock_when:status=posted", true, LockCondition{JSONName: "status", Value: "posted"}},
		{"lock_when:status=void", true, LockCondition{JSONName: "status", Value: "void"}},
		{"lock_when:approved=true", true, LockCondition{JSONName: "approved", Value: "true"}},
		{"lock_when:level=10", true, LockCondition{JSONName: "level", Value: "10"}},
		{"lock_when:status =posted", true, LockCondition{JSONName: "status", Value: "posted"}}, // whitespace tolerated
		{"lock_when:=posted", false, LockCondition{}},                                          // empty field
		{"lock_when:status=", false, LockCondition{}},                                          // empty value
		{"lock_when:status", false, LockCondition{}},                                           // no '='
	}
	for _, c := range cases {
		got, ok := parseLockWhen(c.in)
		if ok != c.ok {
			t.Errorf("parseLockWhen(%q): ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if got != c.want {
			t.Errorf("parseLockWhen(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestLockCondition_MatchesRecord(t *testing.T) {
	lc := LockCondition{JSONName: "status", Value: "posted"}
	if !lc.matchesRecord(map[string]any{"status": "posted"}) {
		t.Error("matchesRecord(posted) should be true")
	}
	if lc.matchesRecord(map[string]any{"status": "draft"}) {
		t.Error("matchesRecord(draft) should be false")
	}
	// Numeric fields stringify cleanly.
	lcNum := LockCondition{JSONName: "level", Value: "10"}
	if !lcNum.matchesRecord(map[string]any{"level": 10}) {
		t.Error("matchesRecord with int(10) vs '10' should match")
	}
	// Missing field is never a match.
	if lc.matchesRecord(map[string]any{}) {
		t.Error("matchesRecord on missing field should be false")
	}
	// nil value never matches.
	if lc.matchesRecord(map[string]any{"status": nil}) {
		t.Error("matchesRecord on nil value should be false")
	}
}

type lockedInvoice struct {
	BaseModel
	Status string `mfx:"enum:draft|posted|void,lock_when:status=posted,lock_when:status=void"`
	Amount float64
}

func TestCollectLockWhen_AggregatesAcrossFields(t *testing.T) {
	meta, err := ScanModel(lockedInvoice{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	if got := len(meta.LockWhen); got != 2 {
		t.Fatalf("LockWhen length = %d, want 2 (posted + void)", got)
	}
	seen := map[string]bool{}
	for _, lc := range meta.LockWhen {
		if lc.JSONName != "Status" && lc.JSONName != "status" {
			t.Errorf("condition references unexpected field: %q", lc.JSONName)
		}
		seen[lc.Value] = true
	}
	if !seen["posted"] || !seen["void"] {
		t.Errorf("missing values; got %v", seen)
	}
}

type lockedBadRef struct {
	BaseModel
	Status string `mfx:"lock_when:satus=posted"` // typo
}

func TestCollectLockWhen_RejectsUnknownField(t *testing.T) {
	_, err := ScanModel(lockedBadRef{}, ModelConfig{})
	if err == nil {
		t.Fatal("ScanModel should reject lock_when referencing an unknown field")
	}
}

type unlocked struct {
	BaseModel
	Name string
}

func TestCollectLockWhen_NoneIsZeroLength(t *testing.T) {
	meta, err := ScanModel(unlocked{}, ModelConfig{})
	if err != nil {
		t.Fatalf("ScanModel: %v", err)
	}
	if len(meta.LockWhen) != 0 {
		t.Errorf("LockWhen should be empty for model with no conditions, got %v", meta.LockWhen)
	}
}
