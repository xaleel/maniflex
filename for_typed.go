package maniflex

// for_typed.go — typed public API over the record carrier (typed-models
// migration, Phase 5 / T5.1). These generic helpers let middleware read the
// request's deserialized body as a concrete *T instead of poking at maps:
//
//	server.Pipeline.Service.Register(
//	    maniflex.Handle(func(ctx *maniflex.ServerContext, u *User) error {
//	        if u.Age < 18 { ctx.Abort(422, "TOO_YOUNG", "must be 18+") }
//	        return nil
//	    }),
//	    maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
//	)
//
// The record is the value the Deserialize step bound from the request body
// (ctx.Record). During the transition the persisted write still derives from
// ctx.ParsedBody, so mutating the returned *T does not yet change what is stored
// — use these for typed reads/validation/side-effects. Full write-from-record
// lands when the pipeline write path is migrated.

import "fmt"

// For returns the request's typed record (*T) and true when one is bound to
// ctx.Record, or (nil, false) otherwise — e.g. a read with no body, a model
// whose body failed to decode, or a T that doesn't match the bound record.
func For[T any](ctx *ServerContext) (*T, bool) {
	if ctx == nil {
		return nil, false
	}
	rec, ok := ctx.Record.(*T)
	return rec, ok
}

// Bind is For with an error instead of a bool, for handlers that require a typed
// body and want to return early on its absence.
func Bind[T any](ctx *ServerContext) (*T, error) {
	if rec, ok := For[T](ctx); ok {
		return rec, nil
	}
	return nil, fmt.Errorf("maniflex: no *%T record bound to this request", *new(T))
}

// Handle adapts a typed handler into a MiddlewareFunc. When a *T record is bound
// to the request, fn runs with it before next(); when none is bound (e.g. a read
// operation), fn is skipped and the chain continues. A non-nil error from fn
// short-circuits the pipeline exactly like any middleware error.
func Handle[T any](fn func(ctx *ServerContext, record *T) error) MiddlewareFunc {
	return func(ctx *ServerContext, next func() error) error {
		if rec, ok := For[T](ctx); ok {
			if err := fn(ctx, rec); err != nil {
				return err
			}
		}
		return next()
	}
}
