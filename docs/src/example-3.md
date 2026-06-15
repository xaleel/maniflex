# Example 3: Order Processing System

This example assembles every advanced topic into one application — actions,
raw queries, query models, a transactional outbox, and a background worker.
The domain is small (orders and inventory) so the integration is visible.

## Domain

```go
type Product struct {
    maniflex.BaseModel
    Name     string  `json:"name"     mfx:"required,filterable,sortable"`
    Price    float64 `json:"price"    mfx:"required,min:0"`
    Stock    int64   `json:"stock"    mfx:"required,min:0,filterable"`
}

type Order struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    CustomerID string  `json:"customer_id" mfx:"required,filterable,immutable"`
    Total      float64 `json:"total"       mfx:"required,min:0,filterable,sortable"`
    Status     string  `json:"status"      mfx:"required,enum:pending|paid|shipped|cancelled,default:pending,filterable,sortable"`

    Lines []OrderLine `json:"lines,omitempty"`
}

type OrderLine struct {
    maniflex.BaseModel
    OrderID   string  `json:"order_id"   mfx:"required,filterable,immutable"`
    ProductID string  `json:"product_id" mfx:"required,filterable,immutable"`
    Quantity  int64   `json:"quantity"   mfx:"required,min:1"`
    UnitPrice float64 `json:"unit_price" mfx:"required,min:0"`
}

// Transactional outbox row — appended in the same transaction as the order.
type OutboxEvent struct {
    maniflex.BaseModel
    Kind      string         `json:"kind"      mfx:"required,filterable"`
    Payload   map[string]any `json:"payload"   mfx:"required"`
    Status    string         `json:"status"    mfx:"required,enum:pending|done|failed,default:pending,filterable"`
    ErrorMsg  string         `json:"error_msg" mfx:"filterable"`
}
```

## Action: place an order atomically

`POST /orders` would normally just insert a row. Real order placement needs:
locking the products, decrementing stock, creating the order and lines,
queueing payment — all in one transaction. An [action endpoint](advanced/actions.md)
handles this explicitly:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/place",
    Handler: placeOrder,
    Middleware: []maniflex.MiddlewareFunc{auth.JWTAuth(secret)},
})

func placeOrder(ctx *maniflex.ServerContext) error {
    var req struct {
        Lines []struct {
            ProductID string `json:"product_id"`
            Quantity  int64  `json:"quantity"`
        } `json:"lines"`
    }
    if err := ctx.BindJSON(&req); err != nil {
        return nil
    }

    tx, err := ctx.BeginTx(ctx.Ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()
    ctx.Tx = tx

    // Reserve stock under a row lock for each product.
    var total float64
    type line struct {
        productID string
        qty       int64
        unit      float64
    }
    var lines []line
    for _, l := range req.Lines {
        p, err := ctx.LockForUpdate("Product", l.ProductID)
        if err != nil {
            return err
        }
        stock := p["stock"].(int64)
        if stock < l.Quantity {
            ctx.Abort(http.StatusConflict, "OUT_OF_STOCK",
                fmt.Sprintf("product %s has %d in stock", l.ProductID, stock))
            return nil
        }
        if _, err := ctx.GetModel("Product").Update(l.ProductID, map[string]any{
            "stock": stock - l.Quantity,
        }); err != nil {
            return err
        }
        unit := p["price"].(float64)
        total += unit * float64(l.Quantity)
        lines = append(lines, line{l.ProductID, l.Quantity, unit})
    }

    order, err := ctx.GetModel("Order").Create(map[string]any{
        "customer_id": ctx.Auth.UserID,
        "total":       total,
        "status":      "pending",
    })
    if err != nil {
        return err
    }

    for _, l := range lines {
        if _, err := ctx.GetModel("OrderLine").Create(map[string]any{
            "order_id":   order["id"],
            "product_id": l.productID,
            "quantity":   l.qty,
            "unit_price": l.unit,
        }); err != nil {
            return err
        }
    }

    // Outbox row — picked up by the background worker after commit.
    if _, err := ctx.GetModel("OutboxEvent").Create(map[string]any{
        "kind":    "charge-payment",
        "payload": map[string]any{"order_id": order["id"], "amount": total},
        "status":  "pending",
    }); err != nil {
        return err
    }

    if err := tx.Commit(); err != nil {
        return err
    }

    ctx.Response = &maniflex.APIResponse{
        StatusCode: http.StatusCreated,
        Data:       order,
    }
    return nil
}
```

If any step fails, the deferred `Rollback` reverts the order, the lines, and
the stock decrement together.

## Query model: revenue report

The dashboard wants `GET /revenue` to return revenue per day. A
[query model](advanced/raw-queries.md) makes this a regular API endpoint:

```go
type Revenue struct {
    maniflex.BaseModel
    Day   string  `json:"day"   mfx:"filterable,sortable"`
    Total float64 `json:"total" mfx:"sortable"`
}

server.MustRegister(Revenue{}, maniflex.ModelConfig{
    QueryModel: &maniflex.QueryModelSpec{
        SQL: `SELECT date(created_at) AS day, SUM(total) AS total
                FROM orders
               WHERE status IN ('paid', 'shipped')
               GROUP BY day`,
    },
})
```

Clients call `GET /revenues?sort=day:desc&limit=30` and get the last 30 days
of revenue, paginated and filterable.

## Background worker: process the outbox

A separate goroutine (or process) sweeps `OutboxEvent`, processes each event,
and updates its status. The worker uses the same registered models:

```go
func runOutboxWorker(server *maniflex.Server) {
    events := server.ModelAccessor("OutboxEvent")
    for range time.Tick(2 * time.Second) {
        rows, _ := events.List(&maniflex.QueryParams{
            Filters: []*maniflex.FilterExpr{{
                Field: "status", Operator: maniflex.OpEq, Value: "pending",
            }},
            Limit: 20,
        })
        for _, ev := range rows {
            if err := process(ev); err != nil {
                events.Update(ev["id"].(string), map[string]any{
                    "status":    "failed",
                    "error_msg": err.Error(),
                })
                continue
            }
            events.Update(ev["id"].(string), map[string]any{"status": "done"})
        }
    }
}
```

The worker is part of the same binary in this example. In production, a
satellite from `jobs/redis` (see [Events & Background Jobs](advanced/events-jobs.md))
replaces the polling loop with a durable queue and at-least-once delivery.

## What this example tied together

- **Actions** for endpoints that don't fit standard CRUD (`/orders/place`).
- **`LockForUpdate`** to safely decrement stock under contention.
- **`maniflex.BeginTx`** so the order, lines, and stock change commit atomically.
- **The transactional outbox pattern** for crossing the boundary between the
  request transaction and external side effects.
- **A query model** for the revenue report — a stable, filterable read
  endpoint built from raw SQL.
- **A background worker** consuming a registered model as its work queue.

Each piece is documented on its own page in the Advanced section:
[actions](advanced/actions.md), [raw queries](advanced/raw-queries.md),
[batch-saga](advanced/batch-saga.md), and
[events-jobs](advanced/events-jobs.md).
