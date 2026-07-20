package redis

import (
	"context"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// streamOps is the set of Redis stream commands the consumer needs.
//
// It exists so the consumer loop can be driven by a fake in tests. The methods
// return plain values rather than go-redis *Cmd types deliberately: XAutoClaim
// returns an *XAutoClaimCmd whose fields are unexported and which has no
// NewXAutoClaimCmdResult constructor, so a fake cannot produce one — while
// ([]XMessage, string, error) it can produce trivially.
type streamOps interface {
	// EnsureGroup creates the consumer group and its stream if absent. Calling
	// it for a group that already exists is not an error.
	EnsureGroup(ctx context.Context, stream, group string) error

	// ReadGroup returns messages never delivered to this group, blocking up to
	// block for them to arrive.
	ReadGroup(ctx context.Context, stream, group, consumer string, count int64, block time.Duration) ([]goredis.XStream, error)

	// AutoClaim transfers messages pending longer than minIdle to consumer,
	// starting at start. It returns the claimed messages and the cursor to pass
	// as start next time; "0-0" means the scan wrapped.
	AutoClaim(ctx context.Context, stream, group, consumer string, minIdle time.Duration, start string, count int64) ([]goredis.XMessage, string, error)

	// Ack marks a message as processed, removing it from the pending list.
	Ack(ctx context.Context, stream, group, id string) error
}

// redisStreamOps is the production implementation, backed by a real client.
type redisStreamOps struct{ client *goredis.Client }

func (r redisStreamOps) EnsureGroup(ctx context.Context, stream, group string) error {
	// "$" would skip everything already in the stream; "0" starts at the
	// beginning so a group created after events were published still sees them.
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
