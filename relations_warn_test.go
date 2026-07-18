package maniflex

import (
	"strings"
	"testing"
)

// relationIssueReg builds a registry exercising all four shapes
// collectRelationIssues distinguishes.
func relationIssueReg(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.AddForTest(&ModelMeta{
		Name: "Thread",
		Relations: []RelationMeta{
			// unregistered target → gated on Config.Strict
			{FieldName: "OwnerID", RelatedModel: "Owner", Convention: true, Kind: BelongsTo},
			// registered target → never reported
			{FieldName: "AccountID", RelatedModel: "Account", Convention: true, Kind: BelongsTo},
			// explicit (non-convention) → never reported
			{FieldName: "ManagerID", RelatedModel: "Ghost", Convention: false, Kind: BelongsTo},
			// no "ID" suffix → always an error, target was guessed from the whole name
			{FieldName: "Author", RelatedModel: "Author", Convention: true, Kind: BelongsTo},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddForTest(&ModelMeta{Name: "Account"}); err != nil {
		t.Fatal(err)
	}
	// Registered, so the Author relation's only problem is its missing "ID"
	// suffix — keeping the two rules independently observable.
	if err := reg.AddForTest(&ModelMeta{Name: "Author"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// A relation on a field with no "ID" suffix is an error regardless of strict
// mode: the target was inferred from the whole field name, so the relation
// resolves against a model that almost certainly does not exist.
func TestCollectRelationIssues_MissingIDSuffixAlwaysFails(t *testing.T) {
	var issues issueList
	collectRelationIssues(relationIssueReg(t), false, &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("a relation on a non-ID field must be reported without Config.Strict")
	}
	out := err.Error()
	if !strings.Contains(out, "Author") {
		t.Errorf("error should name the offending field: %q", out)
	}
	if !strings.Contains(out, "relation:Target") {
		t.Errorf("error should name the fix (mfx:\"relation:Target\"): %q", out)
	}
	// The dangling target is legal by default and must not appear.
	if strings.Contains(out, "OwnerID") {
		t.Errorf("an unregistered target must be silent without Config.Strict: %q", out)
	}
	if len(issues) != 1 {
		t.Errorf("expected exactly 1 issue without strict, got %d: %q", len(issues), out)
	}
}

// An unregistered target is defensible — the field may be a plain foreign id —
// so it is reported only under Config.Strict.
func TestCollectRelationIssues_DanglingTargetIsStrictOnly(t *testing.T) {
	var issues issueList
	collectRelationIssues(relationIssueReg(t), true, &issues)

	out := issues.err().Error()
	if !strings.Contains(out, "OwnerID") || !strings.Contains(out, "Owner") {
		t.Fatalf("strict mode should report the dangling OwnerID→Owner relation: %q", out)
	}
	if !strings.Contains(out, "Config.Strict") {
		t.Errorf("a strict-only issue must be marked as such, so nobody hunts for a bug in "+
			"configuration that is legal by default: %q", out)
	}
	if len(issues) != 2 {
		t.Errorf("expected 2 issues under strict (non-ID suffix + dangling), got %d: %q", len(issues), out)
	}
}

// Neither a registered target nor an explicitly-declared relation is ever a
// problem.
func TestCollectRelationIssues_ValidRelationsAreSilent(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var issues issueList
		collectRelationIssues(relationIssueReg(t), strict, &issues)
		out := ""
		if err := issues.err(); err != nil {
			out = err.Error()
		}
		if strings.Contains(out, "AccountID") {
			t.Errorf("strict=%v: a registered target must not be reported: %q", strict, out)
		}
		if strings.Contains(out, "Ghost") {
			t.Errorf("strict=%v: an explicit (non-convention) relation must not be reported: %q", strict, out)
		}
	}
}
