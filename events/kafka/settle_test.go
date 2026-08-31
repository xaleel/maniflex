package kafka

// The commit rule is this adapter's one deliberate departure from the shared
// settle contract, and it is the cell of the delivery matrix in
// docs/src/advanced-topics/events-jobs.md that separates Kafka from Redis and
// NATS. It lived as an inline `||` reading like a typo; naming it makes the
// departure reviewable, and these cases make it un-changeable in silence.
//
//	go test ./events/kafka/ -run TestShouldCommit

import (
	"context"
	"errors"
	"testing"
)

func TestShouldCommit(t *testing.T) {
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
			why: "the event was disposed of before the shutdown; withholding the " +
				"commit would replay it on restart for nothing",
		},
		{
			name: "unsettled while running", settled: false, ctxErr: nil, want: true,
			why: "Kafka commits are cumulative, so a gap left by a consumer that " +
				"keeps running stalls every later commit on the partition and grows " +
				"the tracker's pending map without bound — this is the departure " +
				"from Redis and NATS, which withhold here and redeliver",
		},
		{
			name: "unsettled during shutdown", settled: false, ctxErr: context.Canceled, want: false,
			why: "there is no later commit to stall, so the offset stays uncommitted " +
				"and the event replays on restart rather than being lost to a " +
				"shutdown between two attempts",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCommit(tc.settled, tc.ctxErr); got != tc.want {
				t.Errorf("shouldCommit(settled=%v, ctxErr=%v) = %v, want %v — %s",
					tc.settled, tc.ctxErr, got, tc.want, tc.why)
			}
		})
	}
}

// Any cancellation cause behaves alike: the rule turns on whether the consumer
// is going away, not on why.
func TestShouldCommit_TreatsEveryCancellationCauseAlike(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("closed")} {
		if shouldCommit(false, err) {
			t.Errorf("shouldCommit(false, %v) = true, want false", err)
		}
	}
}
