// Package accessdecision lets the framework's own middleware tell
// Server.ValidateProduction that it sits on the Auth step without deciding who
// may call anything.
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
// an API applications could use to switch the audit off.
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
