package maniflex

// 11D.8 — DBAdapter is compared by identity, and nothing in the type system says
// so.
//
// Batcher.For and ServerContext.getModel both use == / != on DBAdapter values to
// decide whether two models share a database — that is what stops a transaction
// silently spanning two of them. Two properties an implementation must have for
// that to work, neither enforced by the compiler:
//
//   - comparable at all, or the comparison panics at run time;
//   - distinct per instance, or two separate databases compare equal.
//
// A pointer receiver satisfies both, which is why every built-in adapter uses
// one. These tests pin the consequences rather than the convention, so a change
// to how adapters are compared has to confront them.

import "testing"

// ptrAdapter is the shape every real adapter has: methods on a pointer, so each
// instance is its own identity.
type ptrAdapter struct {
	DBAdapter // embedded nil interface: this test only ever compares, never calls
	dsn       string
}

// valueAdapter is the shape the contract warns against — a value type whose
// fields decide equality.
type valueAdapter struct {
	DBAdapter
	dsn string
}

func TestDBAdapter_PointerInstancesAreDistinct(t *testing.T) {
	// Same DSN, two pools. They are different databases as far as any
	// transaction is concerned, and must not compare equal.
	a := &ptrAdapter{dsn: "file:app.db"}
	b := &ptrAdapter{dsn: "file:app.db"}

	var x, y DBAdapter = a, b
	if x == y {
		t.Error("two separately constructed adapters compare equal — a transaction " +
			"could span two connection pools without the framework noticing")
	}
	if x != DBAdapter(a) {
		t.Error("an adapter does not compare equal to itself")
	}
}

// TestDBAdapter_ValueTypeCollapses documents why the contract says "pointer
// receiver". It is not a style preference: two independent value-type adapters
// with equal fields are indistinguishable, so ResolveAdapter's caller would
// conclude two databases are one.
func TestDBAdapter_ValueTypeCollapses(t *testing.T) {
	var x, y DBAdapter = valueAdapter{dsn: "file:app.db"}, valueAdapter{dsn: "file:app.db"}
	if x != y {
		t.Skip("value adapters compared unequal; the hazard this documents is gone")
	}
	t.Log("value-type adapters with equal fields compare equal — this is the " +
		"collapse the DBAdapter godoc warns about, shown rather than asserted")
}

// TestResolveAdapter_Identity: the per-model override must be returned as-is,
// since callers compare the result. Returning a copy or a wrapper would break
// every == that decides transaction scope.
func TestResolveAdapter_Identity(t *testing.T) {
	global := &ptrAdapter{dsn: "global"}
	perModel := &ptrAdapter{dsn: "per-model"}

	withOverride := &ModelMeta{Adapter: perModel}
	if got := withOverride.ResolveAdapter(global); got != DBAdapter(perModel) {
		t.Error("ResolveAdapter must return the override itself, not a copy — " +
			"callers compare the result by identity")
	}

	plain := &ModelMeta{}
	if got := plain.ResolveAdapter(global); got != DBAdapter(global) {
		t.Error("ResolveAdapter must return the global itself when no override is set")
	}
	if got := plain.ResolveAdapter(nil); got != nil {
		t.Errorf("ResolveAdapter(nil) with no override = %v, want nil", got)
	}
}
