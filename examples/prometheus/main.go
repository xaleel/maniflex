// Command prometheus is the full wiring for scraping maniflex request metrics.
//
// maniflex ships no Prometheus exporter: metrics leave through
// response.Metrics, a RequestObserver that writes into whatever
// response.MetricsCollector you give it, so the framework never depends on a
// metrics library and you are free to use any. What that leaves out is a worked
// example of the other side of the interface, which is this file.
//
// It compiles in CI, so it cannot rot the way an example living only in prose
// can. The docs include it directly — see docs/src/middleware-catalogue/response.md.
package main

import (
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/response"
)

// Order is a stand-in for a real model.
type Order struct {
	maniflex.BaseModel
	Reference string `json:"reference" db:"reference" mfx:"required"`
}

// ANCHOR: collector

// promCollector adapts prometheus/client_golang to response.MetricsCollector.
//
// The two sides disagree about when labels are fixed: MetricsCollector passes a
// label map with every observation, while a Prometheus vector binds its label
// names when it is constructed. So a vector is built on first sight of a metric
// name, its label names taken from that first observation, and every later
// observation is projected onto those names — missing keys become empty, extra
// keys are dropped. Prometheus rejects a metric whose label set varies between
// samples, so projecting is what keeps the exposition valid.
type promCollector struct {
	reg *prometheus.Registry

	mu         sync.Mutex
	counters   map[string]*prometheus.CounterVec
	histograms map[string]*prometheus.HistogramVec
	labelNames map[string][]string
}

func newPromCollector(reg *prometheus.Registry) *promCollector {
	return &promCollector{
		reg:        reg,
		counters:   map[string]*prometheus.CounterVec{},
		histograms: map[string]*prometheus.HistogramVec{},
		labelNames: map[string][]string{},
	}
}

// namesFor returns the label names bound to metric on its first observation.
func (c *promCollector) namesFor(metric string, labels map[string]string) []string {
	if names, ok := c.labelNames[metric]; ok {
		return names
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names) // stable order, so the values line up on every call
	c.labelNames[metric] = names
	return names
}

// valuesFor projects labels onto the names this metric was created with.
func valuesFor(names []string, labels map[string]string) []string {
	values := make([]string, len(names))
	for i, n := range names {
		values[i] = labels[n]
	}
	return values
}

func (c *promCollector) IncCounter(name string, labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := c.namesFor(name, labels)
	vec, ok := c.counters[name]
	if !ok {
		vec = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: name, Help: "maniflex " + name}, names)
		c.reg.MustRegister(vec)
		c.counters[name] = vec
	}
	vec.WithLabelValues(valuesFor(names, labels)...).Inc()
}

func (c *promCollector) ObserveHistogram(name string, value float64, labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := c.namesFor(name, labels)
	vec, ok := c.histograms[name]
	if !ok {
		vec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: name,
			Help: "maniflex " + name,
			// response.Metrics records seconds, which is what DefBuckets covers
			// (5ms to 10s). Widen it for an API that streams or exports.
			Buckets: prometheus.DefBuckets,
		}, names)
		c.reg.MustRegister(vec)
		c.histograms[name] = vec
	}
	vec.WithLabelValues(valuesFor(names, labels)...).Observe(value)
}

// ANCHOR_END: collector

// ANCHOR: wiring

func main() {
	registry := prometheus.NewRegistry()
	collector := newPromCollector(registry)

	server := maniflex.New(maniflex.Config{
		PathPrefix:            "/api",
		MaxConcurrentRequests: 64,
	})
	server.MustRegister(Order{})

	// response.Metrics observes at the router level, so it also counts requests
	// rejected during Auth — the ones that never reach a model.
	server.ObserveRequests(response.Metrics(collector))

	// /metrics is not a maniflex route: mount the API under your own router and
	// register the scrape endpoint beside it. Keep it off the public listener,
	// or put an auth middleware in front — the label set names every model and
	// operation your API exposes.
	r := chi.NewRouter()
	r.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	maniflex.Mount(r, server)

	log.Println("API on /api, metrics on /metrics")
	if err := http.ListenAndServe(":8080", r); err != nil {
		log.Fatal(err)
	}
}

// ANCHOR_END: wiring
