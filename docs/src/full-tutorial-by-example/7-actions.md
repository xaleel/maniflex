# 7. Custom Endpoints & Actions

Customers need to *place* orders. A simple `POST /api/orders` would just
insert one row, but real order placement also has to lock stock, create
line items, and queue a downstream notification — all atomically. This is
the textbook use case for a [custom action](../advanced-topics/actions.md).

## The models

Two new entities. Both opt into soft-delete so an audit trail survives.

```go
// models/order.go
type Order struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt

    CustomerID string  `json:"customer_id" mfx:"required,filterable,immutable"`
    Total      float64 `json:"total"       mfx:"required,min:0,filterable,sortable"`
    Status     string  `json:"status"      mfx:"required,enum:pending|paid|shipped|cancelled,default:pending,filterable,sortable"`

    Lines []OrderLine `json:"lines,omitempty"`
}

// models/order_line.go
type OrderLine struct {
    maniflex.BaseModel
    OrderID   string  `json:"order_id"   mfx:"required,filterable,immutable"`
    BookID    string  `json:"book_id"    mfx:"required,filterable,immutable"`
    Quantity  int64   `json:"quantity"   mfx:"required,min:1"`
    UnitPrice float64 `json:"unit_price" mfx:"required,min:0"`
}

// models/outbox.go — Part 8 will consume rows from here.
type OutboxEvent struct {
    maniflex.BaseModel
    Kind     string         `json:"kind"      mfx:"required,filterable"`
    Payload  map[string]any `json:"payload"   mfx:"required"`
    Status   string         `json:"status"    mfx:"required,enum:pending|done|failed,default:pending,filterable"`
    ErrorMsg string         `json:"error_msg"`
}
```

Register them. We also tenancy-scope reads of `Order` to the calling
customer so one user cannot list another's orders:

```go
server.MustRegister(models.Order{}, models.OrderLine{}, models.OutboxEvent{})

server.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    ctx.Query.Filters = append(ctx.Query.Filters, &maniflex.FilterExpr{
        Field: "customer_id", Operator: maniflex.OpEq, Value: ctx.Auth.UserID,
    })
    return next()
}, maniflex.ForModel("Order"), maniflex.ForOperation(maniflex.OpList))
```

## Why an action

The standard `POST /api/orders` would insert one `Order` row and stop. We
need:

1. **Lock the books** so concurrent buyers don't oversell stock.
2. **Decrement stock** on every line.
3. **Create the order**.
4. **Create one `OrderLine` per book**.
5. **Append an outbox row** describing the order, in the same transaction.

A single transaction must cover all five. The Service step on `POST /orders`
sees only the order body — the lines come from the client. We could write
five middleware functions, but a [custom action](../advanced-topics/actions.md) keeps
the transaction obvious and the trimmed pipeline lighter:

```
Auth → action handler → Response
```

`Deserialize`, `Validate`, `Service`, and `DB` are skipped. Our handler does
its own parsing and database work.

## The handler

`actions/orders.go`:

```go
package actions

func PlaceOrder(ctx *maniflex.ServerContext) error {
    var req struct {
        Lines []struct {
            BookID   string `json:"book_id"`
            Quantity int64  `json:"quantity"`
        } `json:"lines"`
    }
    if err := ctx.BindJSON(&req); err != nil {
        return nil
    }
    if len(req.Lines) == 0 {
        ctx.Abort(http.StatusBadRequest, "EMPTY_ORDER", "an order must contain at least one line")
        return nil
    }

    tx, err := ctx.BeginTx(ctx.Ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()
    ctx.Tx = tx

    // 1+2: lock each book row and decrement stock.
    type planned struct {
        bookID    string
        quantity  int64
        unitPrice float64
    }
    var plan []planned
    var total float64

    for _, l := range req.Lines {
        book, err := ctx.LockForUpdate("Book", l.BookID)
        if err != nil {
            ctx.Abort(http.StatusNotFound, "BOOK_NOT_FOUND",
                fmt.Sprintf("book %s does not exist", l.BookID))
            return nil
        }
        stock := book["stock"].(int64)
        if stock < l.Quantity {
            ctx.Abort(http.StatusConflict, "OUT_OF_STOCK",
                fmt.Sprintf("book %s has %d in stock", l.BookID, stock))
            return nil
        }
        if _, err := ctx.GetModel("Book").Update(l.BookID, map[string]any{
            "stock": stock - l.Quantity,
        }); err != nil {
            return err
        }
        price := book["price"].(float64)
        total += price * float64(l.Quantity)
        plan = append(plan, planned{l.BookID, l.Quantity, price})
    }

    // 3: the Order row.
    order, err := ctx.GetModel("Order").Create(map[string]any{
        "customer_id": ctx.Auth.UserID,
        "total":       total,
        "status":      "pending",
    })
    if err != nil {
        return err
    }

    // 4: one OrderLine per book.
    for _, p := range plan {
        if _, err := ctx.GetModel("OrderLine").Create(map[string]any{
            "order_id":   order["id"],
            "book_id":    p.bookID,
            "quantity":   p.quantity,
            "unit_price": p.unitPrice,
        }); err != nil {
            return err
        }
    }

    // 5: outbox row — picked up by the worker in Part 8.
    if _, err := ctx.GetModel("OutboxEvent").Create(map[string]any{
        "kind": "order-placed",
        "payload": map[string]any{
            "order_id":    order["id"],
            "customer_id": ctx.Auth.UserID,
            "total":       total,
        },
        "status": "pending",
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

Three things worth pointing out:

- **`ctx.LockForUpdate`** acquires a row-level write lock that lasts until
  the transaction ends. A concurrent buyer hitting the same book waits at
  that line until we commit or roll back.
- **All five inserts share `ctx.Tx`.** `ctx.GetModel(...).Create` routes
  through the transaction automatically — there is no separate "transactional
  client" to thread.
- **`defer tx.Rollback()`** is safe after a successful `Commit` — rollback
  becomes a no-op once the transaction has been finalised.

## Registering the action

In `main.go`:

```go
server.Action(maniflex.ActionConfig{
    Method:  "POST",
    Path:    "/orders/place",
    Handler: actions.PlaceOrder,
    Middleware: []maniflex.MiddlewareFunc{
        auth.JWTAuth("dev-secret"),  // identity → ctx.Auth
    },
})
```

`auth.JWTAuth` is the same middleware registered globally on Auth in Part 2,
but action middleware runs only for the action itself — handy when the action
needs different auth from the generated routes.

## Trying it

```bash
curl -X POST localhost:8080/api/orders/place \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"lines\":[{\"book_id\":\"$BOOK\",\"quantity\":2}]}"
```

Response:

```json
{
  "data": {
    "id":          "abc123…",
    "customer_id": "user-alice",
    "total":       25.98,
    "status":      "pending",
    ...
  }
}
```

Re-run it until the book runs out, and the next request gets a clean
`409 OUT_OF_STOCK` instead of a partial write.

## Finishing the "must have bought" review check

Part 4 left a stub: only customers who have bought a book may review it. The
join query needs `order_lines` and `orders` — which we now have:

```go
server.Pipeline.Validate.Register(func(ctx *maniflex.ServerContext, next func() error) error {
    bookID, _ := ctx.Field("book_id")
    rows, _ := ctx.RawQuery(
        `SELECT 1
           FROM order_lines ol
           JOIN orders o ON o.id = ol.order_id
          WHERE o.customer_id = ?
            AND ol.book_id    = ?
            AND o.status     IN ('paid','shipped')`,
        ctx.Auth.UserID, bookID,
    )
    if len(rows) == 0 {
        ctx.Abort(http.StatusForbidden, "PURCHASE_REQUIRED",
            "you may only review books you have bought")
        return nil
    }
    return next()
}, maniflex.ForModel("Review"), maniflex.ForOperation(maniflex.OpCreate))
```

## Next

In **[Part 8 — Events & Background Jobs](8-events-jobs.md)** we build the
background worker that consumes outbox rows and emails order receipts.
