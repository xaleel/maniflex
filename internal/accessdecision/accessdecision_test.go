package accessdecision

import "testing"

// constructor stands in for a middleware constructor: every call returns a new
// closure over its own state, all made from one function literal.
func constructor(label string) func() string {
	fn := func() string { return label }
	MarkNotADecision(fn)
	return fn
}

func other(label string) func() string {
	return func() string { return label }
}

// The whole package rests on this: closures made by one literal share a code
// pointer, so marking one marks every instance a constructor returns — whatever
// state each captured — while a different literal, and a wrapper around a
// marked one, are distinct.
func TestMarkFollowsTheLiteralNotTheInstance(t *testing.T) {
	a, b := constructor("a"), constructor("b")
	if a() == b() {
		t.Fatal("precondition: the two instances must capture different state")
	}
	if !IsNotADecision(a) || !IsNotADecision(b) {
		t.Error("an instance returned by a marking constructor is not marked")
	}

	if IsNotADecision(other("c")) {
		t.Error("a closure from an unmarked literal is marked")
	}

	wrapped := func() string { return a() }
	if IsNotADecision(wrapped) {
		t.Error("a wrapper around a marked closure is marked; it has code of its own")
	}
}
