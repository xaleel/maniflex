package rabbitmq

// Audit EV-5: a connection or channel drop closed the delivery channel and the
// consumer goroutine returned silently. The subscription was then dead for the
// life of the process while the app kept serving and looking healthy — a total
// consumer outage indistinguishable from an idle one.
//
// This adapter does not reconnect (amqp091-go connections do not self-heal, and
// New is handed a connection it does not own and cannot redial), so the fix is
// to make the death loud: an ERROR log naming the queue and an optional
// OnSubscriptionClosed callback.
//
// There is no RabbitMQ broker in this environment. What is tested here is the
// part that does not need one — the decision of *when* a closed delivery
// channel is an outage rather than an orderly shutdown, which is where getting
// it wrong is most costly: report a normal Cancel as an outage and operators
// learn to ignore the alert that matters.
//
//	go test ./events/rabbitmq/...

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

// captureBus is a Bus wired only for the reporting path — no connection needed.
func captureBus(onClosed func(string, error)) *Bus {
	return &Bus{opts: Options{OnSubscriptionClosed: onClosed}}
}

// A cancelled context means the caller invoked Cancel. The delivery channel
// closes on that path too, so without this check every orderly shutdown would
// log an outage.
func TestReportSubscriptionClosed_SilentOnCancel(t *testing.T) {
	var called bool
	b := captureBus(func(string, error) { called = true })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	closeErr := make(chan *amqp.Error, 1)
	b.reportSubscriptionClosed(ctx, "maniflex.orders", closeErr)

	if called {
		t.Error("an orderly Cancel was reported as a subscription outage; " +
			"an alert that fires on every clean shutdown is one nobody reads")
	}
}

// The broker's own reason must reach the caller — it is what distinguishes a
// deleted queue from a network drop from a broker restart.
func TestReportSubscriptionClosed_PassesBrokerError(t *testing.T) {
	var gotQueue string
	var gotErr error
	b := captureBus(func(q string, err error) { gotQueue, gotErr = q, err })

	closeErr := make(chan *amqp.Error, 1)
	closeErr <- &amqp.Error{Code: 320, Reason: "CONNECTION_FORCED - broker forced connection closure"}

	b.reportSubscriptionClosed(context.Background(), "maniflex.orders", closeErr)

	if gotQueue != "maniflex.orders" {
		t.Errorf("queue = %q, want %q — the callback must say which subscription died", gotQueue, "maniflex.orders")
	}
	if gotErr == nil {
		t.Fatal("no error passed to the callback")
	}
	var amqpErr *amqp.Error
	if !errors.As(gotErr, &amqpErr) || amqpErr.Code != 320 {
		t.Errorf("error = %v, want the broker's own *amqp.Error (code 320)", gotErr)
	}
}

// A basic.cancel — the queue was deleted out from under the consumer — closes
// deliveries without putting anything on the close channel. Reporting nothing
// there would restore the silent death for one of its likeliest causes.
func TestReportSubscriptionClosed_ReportsWhenBrokerGivesNoReason(t *testing.T) {
	var gotErr error
	var called bool
	b := captureBus(func(_ string, err error) { called, gotErr = true, err })

	b.reportSubscriptionClosed(context.Background(), "maniflex.orders", make(chan *amqp.Error, 1))

	if !called {
		t.Fatal("a delivery channel that closed with no broker error was not reported at all")
	}
	if gotErr == nil {
		t.Error("callback received a nil error; the caller cannot distinguish this from success")
	}
}

// The callback is optional. A Bus without one must still take the reporting
// path (which logs) rather than panicking on a nil func.
func TestReportSubscriptionClosed_NoCallbackIsSafe(t *testing.T) {
	b := &Bus{}
	closeErr := make(chan *amqp.Error, 1)
	closeErr <- &amqp.Error{Code: 501, Reason: "frame error"}
	b.reportSubscriptionClosed(context.Background(), "maniflex.orders", closeErr)
}

// ── pattern translation ──────────────────────────────────────────────────────
// Pure functions, previously untested. The two must agree: a pattern that binds
// on the broker side and then fails the client-side match silently consumes
// nothing, and a queue that binds too widely burns work on events it discards.

func TestPatternToBindingKey(t *testing.T) {
	for _, tc := range []struct{ pattern, want string }{
		{"*", "#"}, // AMQP's "*" is one word; "all routing keys" is "#"
		{"invoice.*", "invoice.*"},
		{"invoice.created", "invoice.created"},
	} {
		if got := patternToBindingKey(tc.pattern); got != tc.want {
			t.Errorf("patternToBindingKey(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

func TestMatchesAny(t *testing.T) {
	for _, tc := range []struct {
		name      string
		patterns  []string
		eventType string
		want      bool
	}{
		{"star matches everything", []string{"*"}, "invoice.created", true},
		{"prefix glob matches", []string{"invoice.*"}, "invoice.created", true},
		{"prefix glob rejects other prefix", []string{"invoice.*"}, "order.created", false},
		{"exact matches", []string{"invoice.created"}, "invoice.created", true},
		{"exact rejects sibling", []string{"invoice.created"}, "invoice.updated", false},
		{"any of several", []string{"order.*", "invoice.*"}, "invoice.created", true},
		{"no patterns matches nothing", nil, "invoice.created", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesAny(tc.patterns, tc.eventType); got != tc.want {
				t.Errorf("matchesAny(%v, %q) = %v, want %v", tc.patterns, tc.eventType, got, tc.want)
			}
		})
	}
}
