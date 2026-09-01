package rabbitmq

// The ack rule is this adapter's departure from the shared settle contract, and
// the cell of the delivery matrix in docs/src/advanced-topics/events-jobs.md
// that separates RabbitMQ from Redis and NATS.
//
// The reason differs from Kafka's. Prefetch is now bounded (blocker B2), so
// withholding is no longer unbounded — it is worse: withheld messages come back
// only when the channel closes, so N unsettled deliveries fill the prefetch
// window and the consumer stops receiving anything at all.
//
//	go test ./events/rabbitmq/ -run TestShouldAck

import (
	"context"
	"errors"
	"testing"
)

func TestShouldAck(t *testing.T) {
	cases := []struct {
		name    string
		settled bool
		ctxErr  error
		want    bool
		why     string
	}{
		{
			name: "settled while running", settled: true, ctxErr: nil, want: true,
			why: "a handled, dead-lettered, or deliberately dropped event has been " +
				"disposed of and must not be redelivered",
		},
		{
			name: "settled during shutdown", settled: true, ctxErr: context.Canceled, want: true,
			why: "the event was disposed of before the shutdown; requeueing it would " +
				"redeliver work that is already done",
		},
		{
			name: "unsettled while running", settled: false, ctxErr: nil, want: true,
			why: "withheld messages return only when the channel closes, so with a " +
				"bounded prefetch a run of unsettled deliveries fills the window and " +
				"the consumer stops receiving anything at all — the departure from " +
				"Redis and NATS, whose per-message acks let them withhold one " +
				"message without blocking the next",
		},
		{
			name: "unsettled during shutdown", settled: false, ctxErr: context.Canceled, want: false,
			why: "nothing accumulates once the channel is closing, and the unacked " +
				"message is requeued rather than lost to a shutdown between attempts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAck(tc.settled, tc.ctxErr); got != tc.want {
				t.Errorf("shouldAck(settled=%v, ctxErr=%v) = %v, want %v — %s",
					tc.settled, tc.ctxErr, got, tc.want, tc.why)
			}
		})
	}
}

func TestShouldAck_TreatsEveryCancellationCauseAlike(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("closed")} {
		if shouldAck(false, err) {
			t.Errorf("shouldAck(false, %v) = true, want false", err)
		}
	}
}
