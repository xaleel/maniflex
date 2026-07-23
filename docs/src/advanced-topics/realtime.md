# Realtime / WebSockets

maniflex is a synchronous request/response framework, but the `realtime` package
ships a first-class **event hub** that pushes domain events to browsers over
WebSocket and Server-Sent Events. It is a pure consumer of the
[event bus](events-jobs.md): producers publish through `events.Emit` exactly as
they would for any other subscriber, and the hub fans those events out to
connected clients.

Nothing about realtime leaks into a CRUD-only app — the hub is mounted by your
own code outside `server.Handler()`, so a blog that never imports `realtime`
pays no websocket dependency, goroutine, or shutdown phase.

## The shape of it

```go
import (
    "github.com/xaleel/maniflex"
    "github.com/xaleel/maniflex/events"
    "github.com/xaleel/maniflex/events/inproc"
    "github.com/xaleel/maniflex/realtime"
)

bus := inproc.New() // or events/redis, events/nats, … for multi-replica

// Producer: every create/update/delete publishes a domain event.
server.Pipeline.DB.Register(
    events.Emit(bus, events.EmitConfig{Source: "billing"}),
    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
    maniflex.AtPosition(maniflex.After),
)

// Consumer: the hub fans those events out to clients.
hub, err := realtime.NewHub(realtime.HubConfig{Bus: bus})
if err != nil {
    log.Fatal(err)
}

r := chi.NewRouter()
r.Mount("/api", server.Handler())
r.Handle("/ws", hub.Handler())     // WebSocket upgrade
r.Handle("/sse", hub.SSEHandler()) // Server-Sent Events fallback
http.ListenAndServe(":8080", r)
```

Removing realtime is a one-line revert: drop the two `r.Handle` lines and the
`events.Emit` registration.

## Topics

Events are addressed by their CloudEvents **type** — a dotted string like
`invoice.created` or `queue.position_changed`. Clients subscribe with glob
patterns (the same matcher the event bus uses):

| Pattern             | Matches                                   |
| ------------------- | ----------------------------------------- |
| `invoice.*`         | `invoice.created`, `invoice.updated`, …   |
| `*.created`         | any `…​.created` event                     |
| `*`                 | every event                               |

`HubConfig.AllowPatterns` is an optional whitelist of subscribable patterns; an
empty list allows any. A client that asks for a forbidden pattern gets a
`FORBIDDEN_PATTERN` error (WS) or a 403 (SSE).

### WebSocket protocol

The client speaks a tiny JSON protocol over the socket:

```
client → server                              server → client
{"op":"subscribe","patterns":["invoice.*"]}  {"op":"ack","subId":"s_1"}
{"op":"unsubscribe","subId":"s_1"}           {"op":"event","subId":"s_1","data":<event>}
{"op":"ping"}                                {"op":"pong"}
                                             {"op":"error","code":"…","msg":"…"}
```

The `data` field is the full [CloudEvents](events-jobs.md) JSON document, so a
browser can parse it with any CE SDK.

### SSE protocol

SSE is push-only and subscribes via query parameters — ideal for corporate
networks that break WebSockets:

```
GET /sse?subscribe=invoice.*&subscribe=queue.position_changed
```

Each event arrives as a standard `data:` frame whose body is the same
CloudEvents JSON.

The SSE response always sets `X-Accel-Buffering: no` — a safe default that stops
NGINX from buffering the stream and delivering events in batches.

## Authentication

Connections are authenticated **once, on connect** (never per message). Supply
an `Authenticator`; the default `AnonymousOnly{}` accepts everyone.

```go
hub, _ := realtime.NewHub(realtime.HubConfig{
    Bus: bus,
    Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
        claims, err := verifyMyJWT(tok)
        if err != nil {
            return nil, err
        }
        return &realtime.Principal{UserID: claims.Sub, TenantID: claims.Tenant, Roles: claims.Roles}, nil
    }),
})
```

`BearerToken` pulls the token from the `Authorization: Bearer …` header, the
`?access_token=` query parameter (browsers can't set headers on `WebSocket()`),
or the `Sec-WebSocket-Protocol: access_token.<token>` subprotocol. `Composite`
tries several authenticators in order.

### Origin checking

Set `Origins` to restrict which web origins may open a connection; leave it
empty (the default) to allow all. It gates **both** transports — the WebSocket
handshake and the SSE stream. This matters most when the hub authenticates from
an ambient credential such as a cookie, where the browser attaches it
automatically: without an Origin check a page on any origin could open a
connection with the victim's credentials (the realtime equivalent of CSRF).

The two transports treat a **missing** `Origin` header differently, and the
difference is dictated by the browser:

- A **WebSocket** handshake always carries `Origin` (RFC 6455), so a missing one
  means the caller is not a browser and is **refused** when `Origins` is set. If
  you connect from a non-browser WebSocket client — a mobile app, a backend
  relay — either send an allowed `Origin` yourself or leave `Origins` empty.
- A same-origin **`EventSource`** sends **no** `Origin` header at all (the Fetch
  standard adds it only to CORS and WebSocket requests), so a missing one is
  **allowed** — otherwise every ordinary same-origin SSE client would break the
  moment you configure `Origins`. A *present* but unlisted origin is refused,
  which is what a cross-origin browser always sends.

Origin is one layer, not the whole story. Because SSE is an ordinary CORS
request, a cross-origin page also cannot *read* the stream unless your app's own
CORS configuration permits it; the `Origins` check additionally stops the
connection being established at all.

### Per-event authorisation

`AllowPatterns` controls which topics a client may subscribe to; `Visibility`
controls which individual events it actually receives. The hook runs once per
(event, client) pair and can also redact the payload:

```go
HubConfig{
    Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
        if e.TenantID != p.TenantID {
            return false, nil // suppress cross-tenant events
        }
        return true, nil
    },
}
```

Return `(true, &copy)` to deliver a transformed event — the hub clones before
mutation so each client sees its own view.

## Heartbeat

Idle connections are kept alive automatically so L7 proxies (ALB, NGINX, with
their typical 30–60s idle timeouts) don't drop them:

- **WebSocket** — the server sends a ping frame every `PingInterval` (default
  30s); compliant clients answer with a pong.
- **SSE** — the server emits a `: keepalive` comment on the same interval.

### Disconnects

A WebSocket connection is served by a read pump and a write pump, and whichever
one first notices the peer is gone closes the socket and stops the other. That
holds for a clean close frame, an abrupt drop (EOF/RST), and a **half-close** —
a client that shuts down its write side but keeps reading. The half-close is
worth naming because nothing about it fails on its own: every ping the server
writes still succeeds, so before v0.3.3 such a connection was never reaped and
its goroutine and `CLOSE_WAIT` socket were held for the life of the process.
Either way `Hub.Stats().Connections` drops as soon as the peer goes away.

A peer that stops answering *without* closing — the half-open connection a
network partition leaves behind — produces no error at all, so it is caught by
a deadline instead. Every WebSocket connection must send something within
`ReadTimeout` (default 2×`PingInterval`, so 60s) or it is closed with
`1001 Going Away`. Any inbound frame refreshes it, including the pong a
compliant client returns for the server's ping, so a connection that is merely
idle is kept alive by the heartbeat alone and never needs application traffic.

This is the one rule that can disconnect a client which is genuinely there: a
hand-rolled client that ignores ping frames now has a connection lifetime of
`ReadTimeout` rather than forever. Answering pings is the fix — RFC 6455
requires it, and every mainstream client library does it for you. Where that
isn't possible, set `ReadTimeout: realtime.ReadTimeoutDisabled`, understanding
that half-open connections then accumulate undetected.

## Resumable streams (`lastEventId`)

By default delivery is ephemeral: a client that disconnects misses whatever was
published while it was away. Enable resume to give clients a replay buffer.

```go
hub, _ := realtime.NewHub(realtime.HubConfig{
    Bus:          bus,
    ResumeBuffer: 1024, // retain the most recent 1024 events for replay
})
```

With resume enabled, every delivered event carries a **cursor**:

- **SSE** — the cursor is the standard `id:` line. On reconnect the browser's
  `EventSource` automatically sends `Last-Event-ID`, and the hub replays
  everything after it before resuming the live stream. (You can also pass
  `?lastEventId=<cursor>` explicitly.)
- **WebSocket** — events include a `"cursor"` field; resume by adding it to your
  subscribe message: `{"op":"subscribe","patterns":["invoice.*"],"after":"<cursor>"}`.

If the cursor is older than the retained buffer (or the hub restarted), the
client receives a **resync** signal — `event: resync` on SSE, `{"op":"resync"}`
on WebSocket — telling it to refetch current state instead of silently missing
events. Across the reconnect seam delivery is at-least-once; because cursors are
monotonic, clients drop anything at or below their last applied cursor.

`ResumeBuffer` installs an in-process ring buffer, so resume works when the
client reconnects to the **same** replica (WebSocket affinity). For
cross-replica resume, supply your own `ResumeStore` (e.g. backed by a Redis
stream) via `HubConfig.ResumeStore`.

## Schema-emitting events (AsyncAPI)

Just as `/openapi.json` lets clients codegen typed REST clients, the hub's event
catalogue can be published as an **AsyncAPI 2.6** document so clients codegen
typed event payloads. Declare it once:

```go
server.RealtimeDoc(maniflex.AsyncAPIConfig{
    Title:   "Billing events",
    Servers: []maniflex.AsyncAPIServerConfig{
        {Name: "ws", URL: "ws://localhost:8080/ws", Protocol: "ws"},
    },
    // Derive invoice.created|updated|deleted channels from registered models:
    AutoModelEvents: true,
    // …and/or declare custom events with a Go struct payload:
    Events: []maniflex.EventDoc{
        {Type: "payment.received", Title: "Payment received", Payload: PaymentReceived{}},
    },
})
```

This mounts `GET {PathPrefix}/asyncapi.json`. The payload struct is reflected
with the same `json` + `mfx` tags as models and actions
([Actions](actions.md)). The endpoint is opt-in — apps that never call
`RealtimeDoc` get no new route.

## Backpressure & slow clients

Each connection has a bounded outbound queue (`SendBuffer`, default 64). A
client that fills it is kicked at once — a WebSocket close
`1013 Try Again Later`, or an SSE disconnect that triggers `EventSource`
reconnection. `Hub.Stats()` exposes the live connection count and cumulative
kick count (counted per kicked client, not per dropped event) for monitoring. A
frame larger than `MaxMessageSize` (default 64 KiB) is rejected with close
`1009`.

**`SendBuffer` is the only knob that matters here**, because fan-out never
waits. Every client is served from one shared goroutine, so a wait on one is a
wait imposed on all of them: before v0.3.3 the hub paused for up to
`SendTimeout` (5s) on a client whose buffer was full, during which *no other
client received anything*, and the events piling up behind it could fill the
bus's own queue — at which point `Publish` began refusing events process-wide,
for every subscriber rather than just the hub. `SendTimeout` is now ignored and
deprecated; raise `SendBuffer` if your clients need more slack.

Dropping the client rather than the event is deliberate. A kicked client
reconnects and, with a [`ResumeStore`](#resumable-streams-lasteventid)
configured, replays from its cursor — so nothing is lost. A client kept
connected while its events were discarded would have a gap it could never learn
about, which is the worse failure.

## Scaling out

The hub is single-process by design; cross-replica fan-out is the bus's job:

- **inproc** (single binary) — one hub, all clients local.
- **redis / nats / kafka** — every replica subscribes to the bus, so an event
  published anywhere reaches local clients on every replica. Pair with a sticky
  load balancer so each client stays on one replica (WebSocket affinity).

The hub does **not** create a consumer group per connection — per-client
filtering happens server-side, downstream of one shared bus subscription, so
broker load doesn't scale with connection count.

## Graceful shutdown

`Hub.Shutdown(ctx)` stops accepting connections, sends a `1001 Going Away` close
to every client, drains in-flight writes until the deadline, then cancels the
bus subscription. Call it alongside `*http.Server.Shutdown` from the same signal
handler — the hub is mounted by your code, so it isn't part of
`server.Shutdown`.

## HubConfig reference

| Field            | Default          | Purpose                                              |
| ---------------- | ---------------- | ---------------------------------------------------- |
| `Bus`            | — (required)     | the `events.Bus` the hub consumes                    |
| `Authenticator`  | `AnonymousOnly{}`| connection auth                                      |
| `Visibility`     | allow-all        | per-event authorisation / redaction                  |
| `AllowPatterns`  | allow-all        | subscribable topic whitelist                         |
| `ResumeStore`    | nil (disabled)   | replay buffer for `lastEventId` resume               |
| `ResumeBuffer`   | 0 (disabled)     | shortcut: install an in-memory store of this size    |
| `PingInterval`   | 30s              | WS ping / SSE keepalive cadence                      |
| `ReadTimeout`    | 2×`PingInterval` | WS dead-peer deadline; `ReadTimeoutDisabled` to opt out |
| `SendBuffer`     | 64               | per-client outbound queue depth                      |
| `SendTimeout`    | —                | **deprecated, ignored**; fan-out no longer waits     |
| `MaxMessageSize` | 64 KiB           | inbound frame size limit                             |
| `Origins`        | allow-all        | allowed `Origin` values for both WS and SSE          |
