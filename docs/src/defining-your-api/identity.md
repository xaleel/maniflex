# Record Identity

Every record in a Maniflex application is identified by one column, `id`, holding
a string. This page states what that means as a v1 contract: what the framework
guarantees, what it assumes, and what it does not support.

The behaviour described here is pinned by `tests/e2e/identity_test.go`.

## The contract

**Identity is a single string column named `id`.** It is contributed by the
embedded `maniflex.BaseModel`; a model that lacks it fails registration. There
is no composite primary key and no alternative identity column.

**The framework generates the value.** On insert, when `id` is empty, the adapter
assigns a **UUIDv4** in canonical lowercase form — `9f8e7d6c-…`, 36 characters,
random. Nothing about the value is meaningful: it is not sequential, not
time-ordered, and carries no tenant, type, or shard information.

**Clients never choose it.** `id` is `mfx:"readonly"`, and the Validate step
strips the column from every write body unconditionally — including a value a
middleware stamped with `ctx.SetField`, which is a deliberate exception to the
rule that server-set values survive. An `"id"` in a `POST` or `PATCH` body is
ignored, not rejected: the request succeeds and the framework's value is used.

**The value never changes.** No generated route reassigns an id. Updates,
restores, and version history all keep the row's original identity.

**On the wire it is an opaque string.** Generated OpenAPI declares both the `id`
property and the `{id}` path parameter as `{"type":"string","format":"uuid"}`,
and the property as `readOnly`. Nothing parses or validates the id in the
request path: an id of any shape that does not match a row produces `404`, never
`400`. Treat ids as opaque on the client side — compare them for equality, do
not order or parse them.

**In the database it is `TEXT PRIMARY KEY`** on every supported driver,
PostgreSQL included. The framework does not use a native `uuid` column type, a
sequence, or an identity column.

**Relations carry the same string.** A foreign key column stores the target
row's id verbatim, and `?include=` resolves relations by string equality.
Foreign key columns are therefore `string` (or `*string` when the relation is
optional).

**Pagination assumes ids are unordered but unique.** Keyset pagination orders by
`(cursor field, id)`, using the id only to break ties so a page boundary is
total. Because v4 ids are random, that tiebreak order is arbitrary — stable
across a walk, but not meaningful. Do not sort by `id` expecting insertion
order; sort by `created_at` (opting in through
[`BaseModelTags`](models.md#querying-the-basemodel-columns)).

**Adapter interfaces are string-typed.** `FindByID`, `Update`, `Delete`, and
their transactional counterparts all take `id string`. A custom adapter
implements that signature; a multi-column key cannot be expressed through it.

## Assigning your own id

Below the request pipeline, the adapter generates an id only when none was
supplied. Code writing through the [model accessor](../advanced-topics/model-accessor.md)
may therefore choose its own:

```go
row, err := ctx.GetModel("Invoice").Create(map[string]any{
    "id":     "INV-2026-0001",
    "amount": 5000,
})
```

The framework itself relies on this: `maniflex.SingletonID` is the fixed `id` of
a [singleton model's](models.md#singleton-models-singleton) single row.

This is supported, with the responsibilities that come with it:

- **Uniqueness is yours.** A collision surfaces as a primary-key constraint
  error from the database, not as a framework-level validation message.
- **URL safety is yours.** The id appears in `/{model}/{id}` paths. Keep it to
  characters that survive a path segment unescaped.
- **The OpenAPI document still says `format: uuid`.** Consumers generating
  clients from the spec may validate against it.
- **It does not reach the HTTP surface.** There is no way for a client, or for
  a middleware on the request path, to supply an id — only server-side code
  writing through the accessor or an adapter directly.

Use it for rows your application names rather than discovers: a singleton, a
fixed configuration row, a record keyed by an external system's identifier.

## Not supported in v1

These are absent by decision, not by oversight. Support for them would be
additive, so applications that need them are not blocked from adopting v1 — but
nothing in v1 should be read as promising them.

| | |
|---|---|
| **Client-supplied ids** | No route or configuration accepts an id from a request. |
| **Composite / multi-column keys** | Unrepresentable through the adapter interface, which is single-string throughout. |
| **Natural keys as *the* identity** | A natural key belongs in its own `mfx:"unique"` column; routes still address the row by `id`. |
| **Integer or auto-increment ids** | `BaseModel.ID` is a `string`. Declaring an integer id column instead is untested and behaves inconsistently across drivers. |
| **Alternative id formats** | UUIDv7, ULID, and prefixed ids are not configurable. There is no id-generator hook. |
| **Native `uuid` columns** | Identity is stored as `TEXT` on every driver. |
| **Lookup by natural key on the id route** | `GET /model/{value}` matches the `id` column only. Use `?filter=slug:eq:value` on the list route. |

## Why this shape

A single opaque string is the narrowest assumption that every other generated
feature can be written against: one route shape, one foreign-key type, one
cursor tiebreaker, one adapter signature. Random v4 ids are also safe to expose
— they leak neither row counts nor creation order, which sequential ids do.

The cost is real and worth stating: random ids cluster poorly in a B-tree index,
so very large tables pay more on insert than they would with a time-ordered id.
If that matters for a specific table, keep the generated id as the primary key
and add your own time-ordered column with an index for the access pattern that
needs it.
