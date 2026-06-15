package otel

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"maniflex"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// newRecorder returns a TracerProvider that records ended spans in memory.
func newRecorder() (*tracetest.SpanRecorder, oteltrace.Tracer) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return sr, tp.Tracer("test")
}

// runPipeline simulates the pipeline executor: it chains the step middlewares
// so each one's next() invokes the following step, exactly as the real
// buildChain does.
func runPipeline(ctx *maniflex.ServerContext, mws ...maniflex.MiddlewareFunc) error {
	var run func(i int) error
	run = func(i int) error {
		if i >= len(mws) {
			return nil
		}
		return mws[i](ctx, func() error { return run(i + 1) })
	}
	return run(0)
}

func spanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func attrValue(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestTracing_RootAndStepSpans_Nest(t *testing.T) {
	sr, tracer := newRecorder()
	prop := propagation.TraceContext{}

	req := httptest.NewRequest("POST", "/widgets", nil)
	ctx := &maniflex.ServerContext{
		Ctx:       context.Background(),
		Request:   req,
		Model:     &maniflex.ModelMeta{Name: "Widget"},
		Operation: maniflex.OpCreate,
		RequestID: "req-123",
	}

	err := runPipeline(ctx,
		rootSpanMW(tracer, prop),
		stepSpanMW(tracer, "db"),
		func(c *maniflex.ServerContext, next func() error) error {
			c.Response = &maniflex.APIResponse{StatusCode: 201}
			return next()
		},
	)
	if err != nil {
		t.Fatalf("pipeline returned error: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	root := spanByName(spans, "Widget.create")
	db := spanByName(spans, "db")
	if root == nil || db == nil {
		t.Fatalf("missing spans: root=%v db=%v", root != nil, db != nil)
	}

	// The db span must be a child of the root span.
	if db.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("db span is not a child of the root span")
	}
	// The root span has no maniflex parent.
	if root.Parent().IsValid() {
		t.Errorf("root span unexpectedly has a parent: %v", root.Parent())
	}

	// Root span carries routing + HTTP attributes.
	if v, ok := attrValue(root, "maniflex.model"); !ok || v.AsString() != "Widget" {
		t.Errorf("maniflex.model = %v (ok=%v), want Widget", v.AsString(), ok)
	}
	if v, ok := attrValue(root, "maniflex.operation"); !ok || v.AsString() != "create" {
		t.Errorf("maniflex.operation = %v, want create", v.AsString())
	}
	if v, ok := attrValue(root, "maniflex.request_id"); !ok || v.AsString() != "req-123" {
		t.Errorf("maniflex.request_id = %v, want req-123", v.AsString())
	}
	if v, ok := attrValue(root, "http.response.status_code"); !ok || v.AsInt64() != 201 {
		t.Errorf("status_code = %v, want 201", v.AsInt64())
	}
	if root.SpanKind() != oteltrace.SpanKindServer {
		t.Errorf("root span kind = %v, want server", root.SpanKind())
	}
}

func TestTracing_JoinsIncomingTrace(t *testing.T) {
	sr, tracer := newRecorder()
	prop := propagation.TraceContext{}

	// A valid W3C traceparent for a remote parent.
	req := httptest.NewRequest("GET", "/widgets/1", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")

	ctx := &maniflex.ServerContext{
		Ctx:       context.Background(),
		Request:   req,
		Model:     &maniflex.ModelMeta{Name: "Widget"},
		Operation: maniflex.OpRead,
	}

	if err := runPipeline(ctx, rootSpanMW(tracer, prop)); err != nil {
		t.Fatalf("pipeline error: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	root := spans[0]
	wantTrace := "0af7651916cd43dd8448eb211c80319c"
	if got := root.SpanContext().TraceID().String(); got != wantTrace {
		t.Errorf("trace id = %s, want %s (span did not join incoming trace)", got, wantTrace)
	}
	if got := root.Parent().SpanID().String(); got != "b7ad6b7169203331" {
		t.Errorf("parent span id = %s, want b7ad6b7169203331", got)
	}
}

func TestTracing_RecordsError(t *testing.T) {
	sr, tracer := newRecorder()
	wantErr := errors.New("boom")

	ctx := &maniflex.ServerContext{
		Ctx:       context.Background(),
		Model:     &maniflex.ModelMeta{Name: "Widget"},
		Operation: maniflex.OpCreate,
	}

	err := runPipeline(ctx,
		rootSpanMW(tracer, propagation.TraceContext{}),
		func(c *maniflex.ServerContext, next func() error) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("pipeline error = %v, want %v", err, wantErr)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error", got)
	}
	if len(spans[0].Events()) == 0 {
		t.Errorf("expected a recorded error event on the span")
	}
}

func TestSpanName(t *testing.T) {
	cases := []struct {
		model string
		op    maniflex.Operation
		want  string
	}{
		{"Widget", maniflex.OpCreate, "Widget.create"},
		{"Widget", "", "Widget"},
		{"", maniflex.OpList, "list"},
		{"", "", "request"},
	}
	for _, c := range cases {
		ctx := &maniflex.ServerContext{Operation: c.op}
		if c.model != "" {
			ctx.Model = &maniflex.ModelMeta{Name: c.model}
		}
		if got := spanName(ctx); got != c.want {
			t.Errorf("spanName(model=%q, op=%q) = %q, want %q", c.model, c.op, got, c.want)
		}
	}
}

func TestMetricsCollector_RecordsCounterAndHistogram(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	c := NewMetricsCollector(mp, "")
	labels := map[string]string{"model": "Widget", "operation": "create", "status": "201"}
	c.IncCounter("maniflex_requests_total", labels)
	c.IncCounter("maniflex_requests_total", labels)
	c.ObserveHistogram("maniflex_request_duration_seconds", 0.5, labels)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	var sawCounter, sawHisto bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				if m.Name == "maniflex_requests_total" {
					sawCounter = true
					if got := data.DataPoints[0].Value; got != 2 {
						t.Errorf("counter value = %d, want 2", got)
					}
				}
			case metricdata.Histogram[float64]:
				if m.Name == "maniflex_request_duration_seconds" {
					sawHisto = true
					if got := data.DataPoints[0].Count; got != 1 {
						t.Errorf("histogram count = %d, want 1", got)
					}
				}
			}
		}
	}
	if !sawCounter {
		t.Errorf("counter metric not recorded")
	}
	if !sawHisto {
		t.Errorf("histogram metric not recorded")
	}
}

func TestInstrument_NilProviders_NoOp(t *testing.T) {
	srv := maniflex.New(maniflex.Config{})
	// Both providers nil: must not panic and must register nothing observable.
	Instrument(srv, Options{})
}

func TestInstrument_RegistersOnPipeline(t *testing.T) {
	sr, _ := newRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	srv := maniflex.New(maniflex.Config{ServiceName: "svc"})
	// Should wire both signals without panicking.
	Instrument(srv, Options{TracerProvider: tp, MeterProvider: mp})
}
