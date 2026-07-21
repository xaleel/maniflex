package redis

import (
	"context"

	goredis "github.com/redis/go-redis/v9"
)

// promoteOps is the seam for the delayed-job promoter, mirroring the streamOps
// seam so the promote path can be driven by a fake in tests without a live Redis.
// It is deliberately separate from streamOps: promotion is a producer / delayed-set
// concern, not part of the consumer crash-recovery path streamOps covers.
type promoteOps interface {
	// PromoteDue atomically moves delayed members whose score is at or below nowMs
	// from the delayed set to the stream, up to count of them, and returns how many
	// it moved. The whole move is one server-side script, so two promoters racing
	// on different instances cannot move the same member twice, and a dropped
	// connection never leaves a member on the stream but still in the delayed set
	// (or the reverse).
	PromoteDue(ctx context.Context, delayed, stream string, nowMs, count int64) (int64, error)
}

// promoteScript moves due delayed members to the stream. Redis runs the whole
// EVAL atomically and single-threaded, which is what serialises concurrent
// promoters: whichever instance's script runs first claims the currently-due
// members (ZREM removes them, XADD appends them), and every other instance's
// ZRANGEBYSCORE then finds them already gone. Replacing the previous per-member
// XADD+ZREM pipeline — which N replicas each ran against the same members, so a
// delayed job was delivered N times, and which a dropped connection could leave
// half-applied. The ZREM result gates the XADD so the append is tied to this
// script being the one that removed the member, not merely to it having been due.
//
// KEYS[1]=delayed ZSET, KEYS[2]=stream; ARGV[1]=now (unix ms), ARGV[2]=max count.
var promoteScript = goredis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, ARGV[2])
local n = 0
for i = 1, #due do
  if redis.call('ZREM', KEYS[1], due[i]) == 1 then
    redis.call('XADD', KEYS[2], '*', 'job', due[i])
    n = n + 1
  end
end
return n
`)

// redisPromoteOps is the production implementation, backed by a real client.
type redisPromoteOps struct{ client *goredis.Client }

func (r redisPromoteOps) PromoteDue(ctx context.Context, delayed, stream string, nowMs, count int64) (int64, error) {
	return promoteScript.Run(ctx, r.client, []string{delayed, stream}, nowMs, count).Int64()
}
