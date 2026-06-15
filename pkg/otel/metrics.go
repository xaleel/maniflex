package otel

import (
	"context"
	"sync"

	"maniflex/middleware/response"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// NewMetricsCollector returns a response.MetricsCollector backed by an OTel
// meter from mp. Use it directly with response.Metrics when you wire the
// Response step yourself instead of calling Instrument:
//
//	c := otel.NewMetricsCollector(meterProvider, "")
//	srv.Pipeline.Response.Register(response.Metrics(c), maniflex.AtPosition(maniflex.After))
//
// scope names the instrumentation scope; "" defaults to "maniflex". Counters
// and histograms are created lazily, once per metric name, and cached.
func NewMetricsCollector(mp metric.MeterProvider, scope string) response.MetricsCollector {
	if scope == "" {
		scope = defaultScope
	}
	return &collector{
		meter:    mp.Meter(scope),
		counters: map[string]metric.Int64Counter{},
		histos:   map[string]metric.Float64Histogram{},
	}
}

// collector bridges response.MetricsCollector to OTel synchronous instruments.
// Instruments are memoised by name; only their creation is mutex-guarded, since
// Add/Record are themselves safe for concurrent use.
type collector struct {
	meter metric.Meter

	mu       sync.Mutex
	counters map[string]metric.Int64Counter
	histos   map[string]metric.Float64Histogram
}

func (c *collector) IncCounter(name string, labels map[string]string) {
	c.counter(name).Add(context.Background(), 1, metric.WithAttributes(toAttrs(labels)...))
}

func (c *collector) ObserveHistogram(name string, value float64, labels map[string]string) {
	c.histogram(name).Record(context.Background(), value, metric.WithAttributes(toAttrs(labels)...))
}

func (c *collector) counter(name string) metric.Int64Counter {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctr, ok := c.counters[name]; ok {
		return ctr
	}
	// The meter returns a no-op instrument on error rather than nil, so the
	// recording path stays safe even if instrument creation fails.
	ctr, _ := c.meter.Int64Counter(name)
	c.counters[name] = ctr
	return ctr
}

func (c *collector) histogram(name string) metric.Float64Histogram {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h, ok := c.histos[name]; ok {
		return h
	}
	h, _ := c.meter.Float64Histogram(name, metric.WithUnit("s"))
	c.histos[name] = h
	return h
}

// toAttrs converts response.Metrics' string labels to OTel attributes.
func toAttrs(labels map[string]string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(labels))
	for k, v := range labels {
		attrs = append(attrs, attribute.String(k, v))
	}
	return attrs
}
