# Outbound Integrations: `pkg/integration`

`maniflex/pkg/integration` is a small toolkit for the integration patterns
that sit beside the framework: calling third-party HTTP APIs, polling
hardware, and receiving signed webhooks. It's not a feature of the
framework — it's three composable types you call from your own code.

## `Caller` — JSON-over-HTTP with retry

```go
import "github.com/xaleel/maniflex/pkg/integration"

var billing = integration.NewCaller("https://api.billing.example.com")

func init() {
    billing.Headers = map[string]string{
        "Authorization": "Bearer " + secrets.Billing,
    }
}
```

```go
// Inside a handler / job / cron tick:
var resp struct {
    InvoiceID string `json:"invoice_id"`
}
err := billing.Post(ctx, "/invoices", map[string]any{
    "amount":  total,
    "patient": id,
}, &resp)
```

- Always JSON in, JSON out. Pass `out=nil` to discard the response body.
- Passing `[]byte` or `string` as the body skips JSON encoding — useful for
  upstreams that demand a specific wire format.
- Retries fire on **network errors**, **HTTP 5xx**, and **HTTP 429** with a
  configurable backoff (`BackoffFn`; default jittered exponential, capped at
  2s). A valid `Retry-After` header takes precedence, capped at 30s. 4xx
  (other than 429) is final.
- Zero-valued settings are safe: `Timeout` defaults to 10s, `MaxRetry` to 3,
  and `MaxResponseBytes` to 4 MiB. Set the corresponding value negative to
  explicitly disable that protection.
- Redirects are same-origin by default so static custom headers such as
  `X-API-Key` cannot be forwarded to another host. An injected `HTTPClient`
  can opt out only by supplying an explicit `CheckRedirect` policy.
- Non-2xx final responses surface as `*integration.ErrHTTPStatus`. Use
  `errors.As` to inspect `StatusCode`, the parsed JSON `Body`, or the raw
  `RawBody`.
- A response exceeding `MaxResponseBytes` returns
  `integration.ErrResponseTooLarge` before JSON decoding, including when
  `out=nil` or the response is non-2xx.
- Always honours the request context — cancel ctx to abort an in-flight
  retry loop.

## `Poller` — periodic background work

```go
// Own a cancellable context and cancel it from your shutdown hook — there is
// no ShutdownContext() on Server; the poller stops when the context you pass
// is cancelled.
pollCtx, stopPolling := context.WithCancel(context.Background())
defer stopPolling() // or call it from your graceful-shutdown path

p := &integration.Poller{
    Interval: 30 * time.Second,
    Fn: func(ctx context.Context) error {
        return terminal.SyncFingerprints(ctx)
    },
}
go p.Start(pollCtx) // dies cleanly when stopPolling() is called
```

A failed tick is logged and the schedule continues — Poller is for
best-effort background work, not workflows where missing a tick is a bug.
For those, use `pkg/jobs`. Set `RunOnStart: true` to fire immediately rather
than waiting one Interval.

## `WebhookReceiver` — HMAC-signed inbound

```go
wh := &integration.WebhookReceiver{
    Secret:               secrets.PaymentWebhook,
    Algorithm:            "sha256", // or "sha512"
    Logger:               logger,   // optional; defaults to slog.Default()
    TimestampHeaderKey:   "X-Webhook-Timestamp",
    TimestampTolerance:   5 * time.Minute,
    ReplayCheck: claimPaymentEventID, // optional atomic shared-store hook
    // Defaults are GitHub-style: X-Hub-Signature-256 + X-Event-Type
}

http.HandleFunc("/hooks/payments", wh.Handler(map[string]integration.WebhookHandler{
    "payment.succeeded": handlePaymentSucceeded,
    "payment.refunded":  handlePaymentRefunded,
}))
```

With `TimestampHeaderKey` enabled, the sender signs
`<raw timestamp>.<raw body>` and the header must be Unix seconds or RFC3339
within `TimestampTolerance` (default 5 minutes). `ReplayCheck` runs only after
that signature and timestamp pass. It should extract an authenticated event ID
from the signed body and atomically claim it in shared storage; return
`integration.ErrWebhookReplay` when it was already claimed. Keep handlers
idempotent as a final defence.

The handler:

1. Reads at most `MaxBodyBytes` (default 1 MiB) from the request body; an extra
   byte rejects the request rather than truncating it.
2. Computes HMAC over the raw body and compares it constant-time to the
   value in `HeaderKey` (or over timestamp + body when enabled). Common
   `algo=hex` prefixes are tolerated.
3. Applies the optional timestamp and replay checks.
4. Looks up the handler by the `EventHeaderKey` value.
5. Calls the handler with the raw body so it can decode whatever shape the
   upstream sends.

Failure modes:

- 400 — body read error
- 401 — missing/mismatching signature or invalid timestamp
- 409 — `ReplayCheck` returned `ErrWebhookReplay`
- 413 — body exceeds `MaxBodyBytes`
- 404 — no handler registered for that event
- 500 — handler or replay storage returned a non-replay error; the client receives
  `internal server error`, while the original error is written to `Logger`

`WebhookReceiver.Handler` panics if `Secret` is empty or `Algorithm` is
neither `sha256` nor `sha512` — both are configuration mistakes worth
catching at startup.
