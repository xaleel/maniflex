# Outbound Integrations: `pkg/integration`

`maniflex/pkg/integration` is a small toolkit for the integration patterns
that sit beside the framework: calling third-party HTTP APIs, polling
hardware, and receiving signed webhooks. It's not a feature of the
framework — it's three composable types you call from your own code.

## `Caller` — JSON-over-HTTP with retry

```go
import "maniflex/pkg/integration"

var billing = &integration.Caller{
    BaseURL: "https://api.billing.example.com",
    Timeout: 10 * time.Second,
    MaxRetry: 3,
    Headers: map[string]string{
        "Authorization": "Bearer " + secrets.Billing,
    },
}

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
  configurable backoff (`BackoffFn`; default linear up to 1s). 4xx (other
  than 429) is final.
- Non-2xx final responses surface as `*integration.ErrHTTPStatus`. Use
  `errors.As` to inspect `StatusCode`, the parsed JSON `Body`, or the raw
  `RawBody`.
- Always honours the request context — cancel ctx to abort an in-flight
  retry loop.

## `Poller` — periodic background work

```go
p := &integration.Poller{
    Interval: 30 * time.Second,
    Fn: func(ctx context.Context) error {
        return terminal.SyncFingerprints(ctx)
    },
}
go p.Start(server.ShutdownContext()) // dies cleanly on shutdown
```

A failed tick is logged and the schedule continues — Poller is for
best-effort background work, not workflows where missing a tick is a bug.
For those, use `pkg/jobs`. Set `RunOnStart: true` to fire immediately rather
than waiting one Interval.

## `WebhookReceiver` — HMAC-signed inbound

```go
wh := &integration.WebhookReceiver{
    Secret:    secrets.PaymentWebhook,
    Algorithm: "sha256", // or "sha512"
    // Defaults are GitHub-style: X-Hub-Signature-256 + X-Event-Type
}

http.HandleFunc("/hooks/payments", wh.Handler(map[string]integration.WebhookHandler{
    "payment.succeeded": handlePaymentSucceeded,
    "payment.refunded":  handlePaymentRefunded,
}))
```

The handler:

1. Reads at most `MaxBodyBytes` (default 1 MiB) from the request body.
2. Computes HMAC over the raw body and compares it constant-time to the
   value in `HeaderKey`. Common `algo=hex` prefixes (GitHub, Stripe) are
   tolerated.
3. Looks up the handler by the `EventHeaderKey` value.
4. Calls the handler with the raw body so it can decode whatever shape the
   upstream sends.

Failure modes:

- 400 — body read error
- 401 — missing or mismatching signature
- 404 — no handler registered for that event
- 500 — handler returned a non-nil error

`WebhookReceiver.Handler` panics if `Secret` is empty or `Algorithm` is
neither `sha256` nor `sha512` — both are configuration mistakes worth
catching at startup.
