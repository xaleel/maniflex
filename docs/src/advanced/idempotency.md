# Idempotency

Idempotency middleware makes `POST` requests safe to retry over a flaky
network. The first request runs the pipeline normally; the response is
cached keyed on the `Idempotency-Key` header. Subsequent requests with
the same key and the same body short-circuit the pipeline and replay
the cached response.

The pattern is borrowed from Stripe's API. This page documents the
shipped implementation in `maniflex/middleware/idempotency`.

## The contract

An `Idempotency-Key` identifies one *logical* operation. Sending the
same key twice with the same body means "I'm retrying — give me the
same result as last time, do not run the operation again." Sending the
same key with a *different* body means "I am confused about my own
state and you should refuse me." Sending no key means "do not apply
idempotency to this request."

```
Idempotency-Key: e3b0c442-98fc-1c14-9afb-...
```

The key is opaque to the framework — any string the client chooses. A
UUID per logical operation is the conventional choice.

## Registering

The middleware lives on the **Deserialize** step at `maniflex.After`
position, scoped to the operations that should be retryable:

```go
import (
    "github.com/xaleel/maniflex"
    "github.com/xaleel/maniflex/middleware/idempotency"
)

server.Pipeline.Deserialize.Register(
    idempotency.Middleware(idempotency.Config{
        Store: maniflex.NewMemoryCache(),
        TTL:   24 * time.Hour,
    }),
    maniflex.ForOperation(maniflex.OpCreate),
    maniflex.AtPosition(maniflex.After),
)
```

Why `After` on Deserialize: the middleware needs `ctx.RawBody` to compute
the body hash, and the default Deserialize handler populates that. Running
after the default ensures the body is present.

`Config`:

| Field | Default | Purpose |
|---|---|---|
| `Store` | required | the cache backend (anything implementing `maniflex.CacheStore`) |
| `TTL` | `24h` | how long a cached response is replayable |
| `KeyFunc` | `ctx.Auth.UserID` then `ctx.Request.RemoteAddr` | derives the per-caller scope |
| `HeaderRequired` | `false` | when `true`, requests without `Idempotency-Key` are rejected with `400` |
| `Locker` | in-process singleflight | serialises concurrent first-misses on the same key. Supply a Redis-SETNX implementation of `idempotency.Locker` for multi-replica deployments; see [Concurrent first-misses](#concurrent-first-misses) |

## The cache key

The cache key is composed of four parts:

```
<KeyFunc(ctx)>:<model>:<operation>:<idempotency-key>
```

- **`KeyFunc(ctx)`** — the per-caller scope. Defaults to the
  authenticated user ID, falling back to the remote IP for anonymous
  requests. Override to use the API token or any other identifier.
- **`model:operation`** — limits a key's effect to one (model, op)
  pair. The same `Idempotency-Key` can be reused safely for, say,
  `POST /api/orders` and `POST /api/refunds` — they are different
  cache keys.
- **`idempotency-key`** — the client-supplied value.

The body hash is **not** part of the key — it is part of the cached
entry and compared on lookup. This is intentional: it lets the
middleware detect "same key, different body" and respond with
`422 IDEMPOTENCY_KEY_REUSED`.

## What gets cached

Only successful responses (2xx). Failed responses are not cached, on
purpose — retrying a failed write is the whole point of idempotency. A
first attempt that 5xx'd should be re-run on the retry, not replayed.

The cached entry carries:

```go
type Entry struct {
    maniflex.APIResponse           // StatusCode, Data, Error, Meta
    BodyHash    string
    StoredAt    time.Time
}
```

- `StatusCode` — replayed verbatim.
- `Data`, `Meta` — replayed verbatim.
- `BodyHash` — SHA-256 of `ctx.RawBody`, used to detect body mismatch.

The replayed response carries the header **`Idempotent-Replayed: true`**
so the client can tell a replay from a fresh execution.

## What happens on each call

| Request | Effect |
|---|---|
| First request with `Idempotency-Key: K` | runs the pipeline; if 2xx, caches the response |
| Repeat with same key, same body | skips the pipeline; replays cached response; adds `Idempotent-Replayed: true` |
| Repeat with same key, **different** body | `422 IDEMPOTENCY_KEY_REUSED` |
| Repeat with same key after TTL | runs the pipeline as if it were the first time |
| Request with no `Idempotency-Key` | passes through (unless `HeaderRequired` is true) |

## Choosing a store

Two implementations cover the common cases.

### `maniflex.NewMemoryCache`

In-process, per-replica:

```go
idempotency.Config{Store: maniflex.NewMemoryCache(), TTL: time.Hour}
```

Suitable for single-replica development. In a multi-replica deployment
each replica has its own cache — a retry routed to a different replica
gets a fresh run, defeating the purpose.

### Redis (or any shared store)

A shared cache backs the middleware across replicas:

```go
import "github.com/xaleel/maniflex/middleware/db/redis"

store := redis.NewCacheStore(redisClient, "idempotency:")
server.Pipeline.Deserialize.Register(
    idempotency.Middleware(idempotency.Config{
        Store: store,
        TTL:   24 * time.Hour,
    }),
    maniflex.ForOperation(maniflex.OpCreate),
    maniflex.AtPosition(maniflex.After),
)
```

`CacheStore` is a four-method interface; any backend that can store a
TTL'd key/value (Redis, Memcached, DynamoDB with TTL) is a drop-in.

## Concurrent first-misses

Two requests carrying the same `Idempotency-Key` and identical bodies that
arrive at exactly the same moment both miss the cache. Without
serialisation, both would run the full pipeline and both would write —
silently breaking the contract that one key represents one logical
operation. The middleware uses a `Locker` to serialise these first-misses.

The default `Locker` is in-process (singleflight-style): the second
goroutine blocks on a channel until the first releases, then re-checks
the cache and replays. This handles single-replica deployments correctly
out of the box.

For multi-replica deployments, supply `Config.Locker` with a backend that
synchronises across processes — typically Redis `SETNX` with a short TTL:

```go
type Locker interface {
    Acquire(ctx context.Context, key string, ttl time.Duration) (acquired bool, release func(), err error)
}
```

`Acquire` returns `acquired=true` to exactly one caller per key per TTL
window. The caller must invoke `release` once the cache entry is written
(or the work has failed). `acquired=false` means another caller holds (or
held) the lock — singleflight-style lockers block first, then return
false so the loser can replay from cache; SETNX-style lockers return
immediately and the loser re-checks the cache itself.

A `Locker` error (e.g. Redis network blip, request context cancelled)
fails open: the middleware runs the pipeline directly, mirroring the
pre-Locker behaviour rather than returning 503. This trades correctness
under partial outages for availability — appropriate for a feature whose
whole purpose is "make retries safe."

## Scoping to specific endpoints

For most APIs, idempotency belongs only on a handful of write endpoints —
payment, order placement, account creation. Scope with `ForModel` so
unrelated `POST` requests are unaffected:

```go
server.Pipeline.Deserialize.Register(
    idempotency.Middleware(idempotency.Config{Store: store}),
    maniflex.ForModel("Payment", "Order"),
    maniflex.ForOperation(maniflex.OpCreate),
    maniflex.AtPosition(maniflex.After),
)
```

The middleware passes through for unscoped requests with no measurable
cost.

## Requiring the header

For endpoints where retries without a key are dangerous, set
`HeaderRequired: true`:

```go
idempotency.Middleware(idempotency.Config{
    Store:          store,
    HeaderRequired: true,
})
```

A scoped registration is the right shape — make the header mandatory on
payment but optional on lower-stakes resources:

```go
server.Pipeline.Deserialize.Register(
    idempotency.Middleware(idempotency.Config{
        Store: store, HeaderRequired: true,
    }),
    maniflex.ForModel("Payment"), maniflex.ForOperation(maniflex.OpCreate),
    maniflex.AtPosition(maniflex.After),
)
```

A missing header on a covered endpoint returns
`400 IDEMPOTENCY_KEY_REQUIRED`.

## Use with custom actions

Action endpoints run a trimmed pipeline that skips Deserialize, so
pipeline-level idempotency does not apply automatically. To get the
same behaviour for an action, include the middleware in the action's
`Middleware` list:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/place",
    Handler: placeOrder,
    Middleware: []maniflex.MiddlewareFunc{
        auth.JWTAuth(secret),
        idempotency.Middleware(idempotency.Config{Store: store}),
    },
})
```

The middleware reads `ctx.RawBody`, so the action handler must call
`ctx.BindJSON` *after* the middleware runs — or read the body bytes
from `ctx.RawBody` directly.

## Edge cases

- **Same key, different body.** Returns `422 IDEMPOTENCY_KEY_REUSED`. The
  contract is that one key represents one logical operation; reusing it
  for a different payload is almost certainly a client bug.
- **Request currently in flight when retry arrives.** The default
  in-process `Locker` (singleflight-style) holds the second goroutine
  until the first finishes, at which point it replays from cache — only
  one pipeline execution runs per process. For multi-replica
  deployments, supply a `Config.Locker` that uses Redis `SETNX` so two
  replicas don't both run the pipeline. See
  [Concurrent first-misses](#concurrent-first-misses) below.
- **Cache eviction before TTL.** The retry runs the pipeline again. This
  is correct behaviour: the cache is a *replay* mechanism, not a
  *deduplication* mechanism. The application's own uniqueness
  constraints handle "this thing was already created."
- **Operation that mutates external state.** Idempotency caches the
  response, not the side effect. A payment that charged a card once on
  the first request will return the same `paid` response on a retry
  without charging again — *because the first request returned with
  the payment recorded as committed*. The action handler is responsible
  for being idempotent against the external system; the middleware just
  prevents the framework from issuing duplicate writes.

## Operational checklist

- One shared `Store` across replicas. Don't use `MemoryCache` in
  multi-replica deployments.
- TTL longer than your client's longest retry window. 24h is generous;
  for mobile clients on flaky networks, 7d is reasonable.
- Scope to the endpoints that benefit. Don't blanket-apply.
- Pair with `HeaderRequired: true` on payment-like endpoints where
  client correctness depends on it.
- Surface the `Idempotent-Replayed` header in client SDKs so consumers
  can tell a replay from a fresh execution.
- Combine with a uniqueness constraint at the DB level for
  defence-in-depth. A retry that hits a different replica after cache
  eviction will run the pipeline; the DB constraint catches the
  duplicate.
