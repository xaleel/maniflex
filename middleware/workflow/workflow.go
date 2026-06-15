// Package workflow provides a Validate-step middleware that enforces
// permitted state transitions on a model. Use it for status-driven
// workflows where only certain transitions are legal — e.g. invoices that
// flow draft → submitted → approved → paid.
//
// Rules are checked against the *current* stored value of the chosen status
// field on update, so every PATCH that touches the field issues one extra
// `ctx.GetModel(...).Read(id)`. Reads are routed through `ctx.Tx` when one
// is active, so the cost is bounded by the request's existing transaction.
//
// On rejection the middleware aborts with `422 INVALID_TRANSITION`. Guard
// errors (e.g. role checks) use the same code; the guard's error string
// becomes the response message.
package workflow

import (
	"errors"
	"fmt"
	"net/http"

	"maniflex"
)

// Machine is a compiled set of allowed transitions on a single status field.
// Build one with New and pass Middleware() to the Validate step.
type Machine struct {
	field      string
	rules      []rule
	initial    map[string]struct{}
	hasInitial bool
}

type rule struct {
	from   string // "" or "*" means "any"
	to     string // "" or "*" means "any"
	guards []Guard
}

// Option mutates a Machine during construction. Returned by Allow, AllowAny,
// and AllowInitial.
type Option func(*Machine)

// New compiles a state-machine for the named status field.
//
//	sm := workflow.New("status",
//	    workflow.Allow("draft",     "submitted"),
//	    workflow.Allow("submitted", "approved", workflow.RequireRole("manager")),
//	    workflow.AllowAny(workflow.RequireRole("admin")),
//	)
//
// `field` is the JSON field name as it appears in `ctx.ParsedBody`.
func New(field string, opts ...Option) *Machine {
	m := &Machine{
		field:   field,
		initial: make(map[string]struct{}),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Allow permits the transition from -> to. `from` may be "*" to match any
// prior state; `to` may be "*" to match any new state. Guards run in order
// after a rule matches; the first guard error rejects the transition.
func Allow(from, to string, guards ...Guard) Option {
	return func(m *Machine) {
		m.rules = append(m.rules, rule{from: from, to: to, guards: guards})
	}
}

// AllowAny is shorthand for Allow("*", "*", guards...). Typical use is an
// admin escape hatch: AllowAny(RequireRole("admin")).
func AllowAny(guards ...Guard) Option {
	return Allow("*", "*", guards...)
}

// AllowInitial declares the set of states a Create may seed the record with.
// When set, OpCreate is allowed only if the body's field value is in the
// set. When not set, any initial state passes.
func AllowInitial(states ...string) Option {
	return func(m *Machine) {
		m.hasInitial = true
		for _, s := range states {
			m.initial[s] = struct{}{}
		}
	}
}

// Middleware returns the Validate-step MiddlewareFunc for this Machine.
func (m *Machine) Middleware() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil || ctx.Model == nil {
			return next()
		}
		switch ctx.Operation {
		case maniflex.OpCreate:
			return m.handleCreate(ctx, next)
		case maniflex.OpUpdate:
			return m.handleUpdate(ctx, next)
		}
		return next()
	}
}

func (m *Machine) handleCreate(ctx *maniflex.ServerContext, next func() error) error {
	to, present := stringField(ctx.ParsedBody.Map(), m.field)
	if !present {
		return next()
	}
	if m.hasInitial {
		if _, ok := m.initial[to]; !ok {
			rejectInitial(ctx, m.field, to, m.initial)
			return nil
		}
	}
	return next()
}

func (m *Machine) handleUpdate(ctx *maniflex.ServerContext, next func() error) error {
	to, present := stringField(ctx.ParsedBody.Map(), m.field)
	if !present {
		// PATCH that doesn't touch the status field — let other middleware run.
		return next()
	}
	if ctx.ResourceID == "" {
		return next()
	}

	current, err := ctx.GetModel(ctx.Model.Name).Read(ctx.ResourceID)
	if err != nil {
		if errors.Is(err, maniflex.ErrNotFound) {
			// Let the DB step produce the standard 404.
			return next()
		}
		ctx.Abort(http.StatusInternalServerError, "WORKFLOW_LOAD_ERROR", err.Error())
		return nil
	}

	from, _ := stringField(current, m.field)

	// Same-state writes are silently allowed — a no-op transition is not an
	// attempted transition.
	if from == to {
		return next()
	}

	matched, guards := m.match(from, to)
	if !matched {
		rejectTransition(ctx, m.field, from, to)
		return nil
	}
	for _, g := range guards {
		if err := g.Check(ctx, from, to); err != nil {
			rejectGuard(ctx, m.field, from, to, err.Error())
			return nil
		}
	}
	return next()
}

// match returns the matched rule's guards and true on the first hit.
func (m *Machine) match(from, to string) (bool, []Guard) {
	for _, r := range m.rules {
		if (r.from == "*" || r.from == from) && (r.to == "*" || r.to == to) {
			return true, r.guards
		}
	}
	return false, nil
}

// stringField coerces a body or record value to string for comparison.
// Returns (value, true) when the field is present and non-nil; (_, false)
// otherwise.
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	return fmt.Sprintf("%v", v), true
}

func rejectTransition(ctx *maniflex.ServerContext, field, from, to string) {
	ctx.Response = &maniflex.APIResponse{
		StatusCode: http.StatusUnprocessableEntity,
		Error: &maniflex.APIError{
			Code: "INVALID_TRANSITION",
			Message: fmt.Sprintf(
				"transition from %q to %q is not permitted", from, to),
			Details: []map[string]string{{
				"field": field, "from": from, "to": to,
			}},
		},
	}
}

func rejectGuard(ctx *maniflex.ServerContext, field, from, to, msg string) {
	ctx.Response = &maniflex.APIResponse{
		StatusCode: http.StatusUnprocessableEntity,
		Error: &maniflex.APIError{
			Code:    "INVALID_TRANSITION",
			Message: msg,
			Details: []map[string]string{{
				"field": field, "from": from, "to": to,
			}},
		},
	}
}

func rejectInitial(ctx *maniflex.ServerContext, field, to string, allowed map[string]struct{}) {
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	ctx.Response = &maniflex.APIResponse{
		StatusCode: http.StatusUnprocessableEntity,
		Error: &maniflex.APIError{
			Code: "INVALID_TRANSITION",
			Message: fmt.Sprintf(
				"initial state %q is not permitted (allowed: %v)", to, keys),
			Details: []map[string]string{{
				"field": field, "from": "", "to": to,
			}},
		},
	}
}
