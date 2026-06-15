// Package otel wires a maniflex Server to OpenTelemetry tracing and metrics.
//
// It is an optional adapter: the maniflex core depends only on chi + uuid and
// never imports OpenTelemetry. This module consumes the OTel *API* packages
// (trace, metric, propagation) and lets the application supply SDK-backed
// providers, so a deployment chooses its own exporter (OTLP, Jaeger, stdout,
// Prometheus, …) without the framework taking a hard dependency on any of it.
//
//	import (
//	    "maniflex"
//	    mfxotel "maniflex/pkg/otel"
//	    "go.opentelemetry.io/otel/sdk/trace"  // your SDK + exporter
//	)
//
//	srv := maniflex.New(cfg)
//	mfxotel.Instrument(srv, mfxotel.Options{
//	    TracerProvider: tracerProvider, // from your configured OTel SDK
//	    MeterProvider:  meterProvider,
//	})
//	srv.MustRegister(User{})
//
// Tracing. Instrument starts one server span per request on the Auth step,
// extracting any incoming W3C trace context so the span joins the caller's
// trace, plus a child span on each subsequent pipeline step (deserialize,
// validate, service, db, response). Because pipeline steps are nested
// middleware — each step's work includes every step after it — the step spans
// nest in pipeline order rather than sitting side by side. A step span
// therefore covers that step plus all steps after it, the same cumulative
// semantics as Config.Trace.Timings. Call Instrument before registering custom
// Auth middleware if you want the server span to wrap that middleware too.
//
// Metrics. Instrument bridges the framework's response.MetricsCollector
// extension point to an OTel meter, recording a maniflex_requests_total counter
// and a maniflex_request_duration_seconds histogram, each labelled with model,
// operation, and status.
package otel

import (
	"context"
	"net/http"

	"maniflex"
	"maniflex/middleware/response"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// defaultScope is the instrumentation scope name used for the tracer and meter
// when Options.ScopeName is empty.
const defaultScope = "maniflex"

// Options configures the OpenTelemetry instrumentation applied by Instrument.
//
// Either provider may be nil to disable that signal; passing both nil makes
// Instrument a no-op.
type Options struct {
	// TracerProvider supplies the tracer used for request and step spans.
	// nil disables tracing.
	TracerProvider trace.TracerProvider

	// MeterProvider supplies the meter used for request counters and the
	// duration histogram. nil disables metrics.
	MeterProvider metric.MeterProvider

	// Propagator extracts trace context from incoming request headers so spans
	// join the caller's trace. Defaults to the W3C TraceContext + Baggage
	// composite when nil.
	Propagator propagation.TextMapPropagator

	// ScopeName names the OTel instrumentation scope for both the tracer and
	// the meter. Defaults to "maniflex" when empty.
	ScopeName string
}

// Instrument registers OpenTelemetry tracing and metrics middleware on the
// server's pipeline according to opts. It is safe to call once, after
// maniflex.New and before Start; see the package doc for ordering notes.
func Instrument(s *maniflex.Server, opts Options) {
	scope := opts.ScopeName
	if scope == "" {
		scope = defaultScope
	}

	if opts.TracerProvider != nil {
		prop := opts.Propagator
		if prop == nil {
			prop = propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{}, propagation.Baggage{},
			)
		}
		tracer := opts.TracerProvider.Tracer(scope)

		// The server span lives on the Auth step; its next() chains through
		// every later step, so it wraps the whole request.
		s.Pipeline.Auth.Register(rootSpanMW(tracer, prop), maniflex.WithName("otel.span"))

		// Child spans for the remaining steps. Auth has no separate child span
		// because the server span already covers the auth step's work.
		s.Pipeline.Deserialize.Register(stepSpanMW(tracer, "deserialize"), maniflex.WithName("otel.span"))
		s.Pipeline.Validate.Register(stepSpanMW(tracer, "validate"), maniflex.WithName("otel.span"))
		s.Pipeline.Service.Register(stepSpanMW(tracer, "service"), maniflex.WithName("otel.span"))
		s.Pipeline.DB.Register(stepSpanMW(tracer, "db"), maniflex.WithName("otel.span"))
		s.Pipeline.Response.Register(stepSpanMW(tracer, "response"), maniflex.WithName("otel.span"))
	}

	if opts.MeterProvider != nil {
		collector := NewMetricsCollector(opts.MeterProvider, scope)
		s.Pipeline.Response.Register(
			response.Metrics(collector),
			maniflex.AtPosition(maniflex.After),
			maniflex.WithName("otel.metrics"),
		)
	}
}

// rootSpanMW returns the Auth-step middleware that opens the per-request server
// span, extracting any incoming trace context so the span joins the caller's
// distributed trace.
func rootSpanMW(tracer trace.Tracer, prop propagation.TextMapPropagator) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		parent := ctx.Ctx
		if parent == nil {
			parent = context.Background()
		}
		if ctx.Request != nil {
			parent = prop.Extract(parent, propagation.HeaderCarrier(ctx.Request.Header))
		}

		spanCtx, span := tracer.Start(parent, spanName(ctx), trace.WithSpanKind(trace.SpanKindServer))
		setRequestAttrs(span, ctx)

		ctx.Ctx = spanCtx
		err := next()
		finishSpan(span, ctx, err)
		ctx.Ctx = parent
		return err
	}
}

// stepSpanMW returns a middleware that opens a child span for one pipeline step.
func stepSpanMW(tracer trace.Tracer, step string) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		parent := ctx.Ctx
		if parent == nil {
			parent = context.Background()
		}

		spanCtx, span := tracer.Start(parent, step)
		ctx.Ctx = spanCtx
		err := next()
		finishSpan(span, ctx, err)
		ctx.Ctx = parent
		return err
	}
}

// spanName builds the server span name from the routed model and operation,
// e.g. "User.create". It falls back gracefully when either is unset.
func spanName(ctx *maniflex.ServerContext) string {
	op := string(ctx.Operation)
	if ctx.Model != nil && ctx.Model.Name != "" {
		if op != "" {
			return ctx.Model.Name + "." + op
		}
		return ctx.Model.Name
	}
	if op != "" {
		return op
	}
	return "request"
}

// setRequestAttrs tags the server span with routing and HTTP attributes.
func setRequestAttrs(span trace.Span, ctx *maniflex.ServerContext) {
	attrs := make([]attribute.KeyValue, 0, 6)
	if ctx.Model != nil && ctx.Model.Name != "" {
		attrs = append(attrs, attribute.String("maniflex.model", ctx.Model.Name))
	}
	if ctx.Operation != "" {
		attrs = append(attrs, attribute.String("maniflex.operation", string(ctx.Operation)))
	}
	if ctx.ResourceID != "" {
		attrs = append(attrs, attribute.String("maniflex.resource_id", ctx.ResourceID))
	}
	if ctx.RequestID != "" {
		attrs = append(attrs, attribute.String("maniflex.request_id", ctx.RequestID))
	}
	if ctx.Request != nil {
		attrs = append(attrs,
			attribute.String("http.request.method", ctx.Request.Method),
			attribute.String("url.path", ctx.Request.URL.Path),
		)
	}
	span.SetAttributes(attrs...)
}

// finishSpan records the outcome (error / 5xx status) on span and ends it.
func finishSpan(span trace.Span, ctx *maniflex.ServerContext, err error) {
	switch {
	case err != nil:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	case ctx.Response != nil && ctx.Response.StatusCode >= http.StatusInternalServerError:
		span.SetStatus(codes.Error, http.StatusText(ctx.Response.StatusCode))
	}
	if ctx.Response != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", ctx.Response.StatusCode))
	}
	span.End()
}
