// Package body provides Deserialize-step middleware for request body control.
package body

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/xaleel/maniflex"
)

// MaxBodySize overrides the default 4 MB body limit for the models this
// middleware is registered on.
//
//	// Allow up to 16 MB on the Article model (large HTML body field)
//	server.Pipeline.Deserialize.Register(
//	    body.MaxBodySize(16<<20),
//	    maniflex.ForModel("Article"),
//	)
//	// Restrict Profile updates to 64 KB
//	server.Pipeline.Deserialize.Register(
//	    body.MaxBodySize(64<<10),
//	    maniflex.ForModel("Profile"),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
//	)
func MaxBodySize(maxBytes int64) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.Request.Body != nil {
			ctx.Request.Body = http.MaxBytesReader(ctx.Writer, ctx.Request.Body, maxBytes)
		}
		if err := next(); err != nil {
			// MaxBytesReader surfaces the limit error through the normal read path;
			// the Deserialize default step turns that into a 400, so we just propagate.
			if isMaxBytesError(err) {
				ctx.Abort(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
					fmt.Sprintf("request body exceeds maximum size of %d bytes", maxBytes))
				return nil
			}
			return err
		}
		return nil
	}
}

func isMaxBytesError(err error) bool {
	// Go 1.19+ wraps this as *http.MaxBytesError; for older versions check message.
	type maxBytesError interface{ Error() string }
	if err == nil {
		return false
	}
	// http.ErrHandlerTimeout and MaxBytesError are the two sentinel errors.
	return err == io.EOF || // may surface as EOF on MaxBytesReader overflow
		err.Error() == "http: request body too large"
}

// StripUnknownFields removes any JSON keys in ctx.ParsedBody that do not
// correspond to a registered field on the model. This prevents clients from
// injecting unexpected keys that might slip through to the database adapter.
//
// Run this Before the default Deserialize step (position Before is the default)
// but it operates on ctx.ParsedBody so it must run After deserialisation — i.e.
// register it on the Validate step instead, or use AtPosition(After) on Deserialize.
//
//	server.Pipeline.Validate.Register(body.StripUnknownFields())
func StripUnknownFields() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		known := make(map[string]bool, len(ctx.Model.Fields))
		for _, f := range ctx.Model.Fields {
			known[f.Tags.JSONName] = true
		}
		for _, key := range ctx.ParsedBody.Keys() {
			if !known[key] {
				ctx.DeleteField(key)
			}
		}
		return next()
	}
}

// CoerceTypes attempts to convert string values in ctx.ParsedBody to the
// Go type declared on the corresponding model field. This is useful when
// consuming HTML form submissions where all values arrive as strings.
//
// Supported coercions: string→int/float64/bool. Unknown types are left as-is.
//
//	server.Pipeline.Validate.Register(body.CoerceTypes())
func CoerceTypes() maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if ctx.ParsedBody == nil {
			return next()
		}
		for _, f := range ctx.Model.Fields {
			jn := f.Tags.JSONName
			raw, ok := ctx.ParsedBody.Get(jn)
			if !ok || raw == nil {
				continue
			}
			str, isStr := raw.(string)
			if !isStr {
				continue
			}

			kind := f.Type.Kind()

			switch {
			case isIntKind(kind):
				if n, err := strconv.ParseInt(str, 10, 64); err == nil {
					ctx.SetField(jn, n)
				}
			case isFloatKind(kind):
				if n, err := strconv.ParseFloat(str, 64); err == nil {
					ctx.SetField(jn, n)
				}
			case kind.String() == "bool":
				if b, err := strconv.ParseBool(str); err == nil {
					ctx.SetField(jn, b)
				}
			}
		}
		return next()
	}
}

func isIntKind(k interface{ String() string }) bool {
	s := k.String()
	return s == "int" || s == "int8" || s == "int16" || s == "int32" || s == "int64" ||
		s == "uint" || s == "uint8" || s == "uint16" || s == "uint32" || s == "uint64"
}

func isFloatKind(k interface{ String() string }) bool {
	s := k.String()
	return s == "float32" || s == "float64"
}
