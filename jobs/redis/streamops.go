package redis

import (
	"context"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// streamOps is the set of Redis stream commands the consumer side needs.
//
// It exists so the Dequeue / reclaim / renew paths can be driven by a fake in
// tests without a live Redis — mirroring the events/redis seam (audit EV-7).
// The methods return plain values rather than go-redis *Cmd types: XAutoClaim
// returns an *XAutoClaimCmd whose fields are unexported and which has no result
// constructor, so a fake cannot build one, whereas ([]XMessage, string, error)
// it can produce trivially. Producer and delayed-set commands stay on the raw
// client — they are not part of the crash-recovery path this seam covers.
type streamOps interface {
	// EnsureGroup creates the consumer group and its stream if absent. Calling
	// it for a group that already exists is not an error.
	EnsureGroup(ctx context.Context, stream, group string) error

	// ReadGroup returns messages never delivered to this group ("new" work),
	// blocking up to block for them to arrive.
	ReadGroup(ctx context.Context, stream, group, consumer string, count int64, block time.Duration) ([]goredis.XStream, error)

	// AutoClaim transfers messages pending longer than minIdle to consumer,
	// starting at start. It returns the claimed messages and the cursor to pass
	// as start next time; "0-0" means the scan wrapped.
	AutoClaim(ctx context.Context, stream, group, consumer string, minIdle time.Duration, start string, count int64) ([]goredis.XMessage, string, error)

	// Ack marks a message processed, removing it from the pending list.
	Ack(ctx context.Context, stream, group, id string) error

	// Claim resets the idle clock of the given pending messages by re-claiming
	// them to consumer with a zero min-idle. It is how a live worker renews its
	// hold on a long-running job so the reclaimer does not mistake it for
	// abandoned. Returns the IDs it actually reset (JUSTID).
	Claim(ctx context.Context, stream, group, consumer string, ids []string) ([]string, error)
}

// redisStreamOps is the production implementation, backed by a real client.
type redisStreamOps struct{ client *goredis.Client }

func (r redisStreamOps) EnsureGroup(ctx context.Context, stream, group string) error {
	err := r.client.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil // already exists — the expected case on every start but the first
	}
	return err
}

func (r redisStreamOps) ReadGroup(ctx context.Context, stream, group, consumer string, count int64, block time.Duration) ([]goredis.XStream, error) {
	return r.client.XReadGroup(ctx, &goredis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
}

func (r redisStreamOps) AutoClaim(ctx context.Context, stream, group, consumer string, minIdle time.Duration, start string, count int64) ([]goredis.XMessage, string, error) {
	return r.client.XAutoClaim(ctx, &goredis.XAutoClaimArgs{
		Stream:   stream,
		Group:    group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    start,
		Count:    count,
	}).Result()
}

func (r redisStreamOps) Ack(ctx context.Context, stream, group, id string) error {
	return r.client.XAck(ctx, stream, group, id).Err()
}

func (r redisStreamOps) Claim(ctx context.Context, stream, group, consumer string, ids []string) ([]string, error) {
	// MinIdle 0 claims regardless of current idle and resets it to 0. JustID
	// avoids shipping the payloads back — a renew only needs the idle reset.
	return r.client.XClaimJustID(ctx, &goredis.XClaimArgs{
		Stream:   stream,
		Group:    group,
		Consumer: consumer,
		MinIdle:  0,
		Messages: ids,
	}).Result()
}
