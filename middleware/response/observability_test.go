package response_test

import (
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/response"
)

type metricsCapture struct {
	counterName   string
	histogramName string
	labels        map[string]string
	duration      float64
}

func (c *metricsCapture) IncCounter(name string, labels map[string]string) {
	c.counterName = name
	c.labels = labels
}

func (c *metricsCapture) ObserveHistogram(name string, value float64, labels map[string]string) {
	c.histogramName = name
	c.duration = value
	c.labels = labels
}

func TestMetricsMapsCompletedRequestObservation(t *testing.T) {
	collector := &metricsCapture{}
	response.Metrics(collector)(maniflex.RequestObservation{
		Duration:  2500 * time.Millisecond,
		Model:     "Order",
		Operation: maniflex.OpCreate,
		Status:    201,
	})

	if collector.counterName != "maniflex_requests_total" {
		t.Errorf("counter = %q", collector.counterName)
	}
	if collector.histogramName != "maniflex_request_duration_seconds" {
		t.Errorf("histogram = %q", collector.histogramName)
	}
	if collector.duration != 2.5 {
		t.Errorf("duration = %v, want 2.5", collector.duration)
	}
	if collector.labels["model"] != "Order" ||
		collector.labels["operation"] != string(maniflex.OpCreate) ||
		collector.labels["status"] != "201" {
		t.Errorf("labels = %#v", collector.labels)
	}
}
