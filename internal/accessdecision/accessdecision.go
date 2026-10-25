// Package accessdecision carries the two things the framework's own middleware
// needs to tell a startup check about itself, neither of which a func value can
// hold on its own.
//
// The first lets middleware tell Server.ValidateProduction that it sits on the
// Auth step without deciding who may call anything. The second lets it say that
// it decides which rows a caller may see, so a check can ask whether that scope
// reaches a given endpoint (see MarkRowScope).
//
// The production audit asks, of every mounted operation, whether someone decided
// who may reach it, and it answers by looking for Pipeline.Auth middleware that
// applies. Most middleware there does decide — it authenticates, or refuses a
// caller without a role — and a middleware an application writes is taken to,
// since the audit cannot see inside it. A few of the framework's do not:
// auth.CSRF compares a cookie to a header, and auth.AllowAnonymous only leaves a
// note for an authenticator that may not exist. Counted as decisions, either one
// alone passed the audit with every route open (audit AUTH-6).
//
// A function value carries no metadata, so the mark is its code pointer. Every
// closure made from one function literal shares that pointer, which is exactly
// the granularity wanted: marking the value a constructor returns marks every
// value it will ever return. A middleware that wraps a marked one has code of its
// own and is not marked — it counts, like any other middleware an application
// writes.
//
// Internal so it stays a contract between the framework's packages rather than
// an API applications could use to switch the audit off. An application's own
// scope middleware says the same thing with maniflex.ProvidesScope(), which is
// public and which the checks read as well.
package accessdecision

import (
	"reflect"
	"sync"
)

var notDecisions sync.Map // code pointer → struct{}

// MarkNotADecision records that fn, and every closure sharing its code, makes no
// access decision. fn must be a func value.
func MarkNotADecision(fn any) {
	notDecisions.Store(reflect.ValueOf(fn).Pointer(), struct{}{})
}

// IsNotADecision reports whether fn shares its code with a function passed to
// MarkNotADecision.
func IsNotADecision(fn any) bool {
	_, ok := notDecisions.Load(reflect.ValueOf(fn).Pointer())
	return ok
}

var rowScopes sync.Map // code pointer → label

// MarkRowScope records that fn decides which rows a caller may see, under the
// given label — "db.Tenancy", "auth.Enforce". fn must be a func value.
//
// The label is what makes a startup warning actionable: "this model's rows are
// not scoped" sends the operator looking, while "db.Tenancy on the DB step does
// not reach GET /search" names the registration to move. There is no way to
// recover a constructor's name from a closure, so the constructor states it.
//
// The mark is not an alternative to maniflex.ProvidesScope(): that declares where
// the middleware should run, and this only says what it is for. A check that
// asks "is this model scoped" reads both — the mark for the framework's own
// middleware whatever way it was registered, and ProvidesScope for an
// application's.
func MarkRowScope(fn any, label string) {
	rowScopes.Store(reflect.ValueOf(fn).Pointer(), label)
}

// RowScopeLabel returns the label fn was marked with, and whether it was marked
// at all.
func RowScopeLabel(fn any) (string, bool) {
	v, ok := rowScopes.Load(reflect.ValueOf(fn).Pointer())
	if !ok {
		return "", false
	}
	label, _ := v.(string)
	return label, true
}
