# Field Schema & Nullability

This page is the frozen contract for the DDL maniflex emits: what a Go field
becomes as a column, and what that column's nullability does and does not tell
you. It exists for the people reading the database from *outside* the
application — a migration script, a reporting replica, BI, another service —
because the encoding is not the one most of them assume.

The short version, and the one thing to take away:

> **`NOT NULL` does not mean "required", and `NULL` is not how maniflex says
> "no value".** A non-pointer field is always `NOT NULL`, and its *zero value* —
> `''`, `0`, `false` — is what "absent" means.

## What a field becomes

For a model whose fields cover every case:

```go
type SchemaSpec struct {
    maniflex.BaseModel

    ReqText string  `db:"req_text" mfx:"required"`
    ReqNum  int     `db:"req_num"  mfx:"required"`

    OptText string  `db:"opt_text"`
    OptNum  int     `db:"opt_num"`
    OptRate float64 `db:"opt_rate"`
    OptFlag bool    `db:"opt_flag"`

    NullText *string `db:"null_text"`
    NullNum  *int    `db:"null_num"`

    Tagged string `db:"tagged" mfx:"default:pending"`
}
```

AutoMigrate emits exactly this:

```sql
CREATE TABLE "schema_specs" (
  "id"         TEXT PRIMARY KEY,
  "created_at" TEXT NOT NULL DEFAULT '0001-01-01T00:00:00Z',
  "updated_at" TEXT NOT NULL DEFAULT '0001-01-01T00:00:00Z',
  "req_text"   TEXT    NOT NULL,
  "req_num"    INTEGER NOT NULL,
  "opt_text"   TEXT    NOT NULL DEFAULT '',
  "opt_num"    INTEGER NOT NULL DEFAULT 0,
  "opt_rate"   REAL    NOT NULL DEFAULT 0,
  "opt_flag"   INTEGER NOT NULL DEFAULT 0,
  "null_text"  TEXT    NULL,
  "null_num"   INTEGER NULL,
  "tagged"     TEXT    NOT NULL DEFAULT 'pending'
)
```

| Field shape | Column | Meaning of "absent" |
|---|---|---|
| non-pointer, `mfx:"required"` | `NOT NULL`, **no DEFAULT** | cannot be absent |
| non-pointer, optional | `NOT NULL DEFAULT <zero>` | the zero value |
| pointer | `NULL` | `NULL` |
| any, `mfx:"default:x"` | `DEFAULT 'x'` | the declared default |

`tests/e2e/schema_contract_test.go` pins every row of that table against the
DDL actually emitted, so this page cannot drift from the code.

## Reading required-ness from the schema

Both a required and an optional non-pointer column are `NOT NULL`. The only
schema-level difference is that **a required column carries no DEFAULT**:

```sql
"req_text" TEXT NOT NULL              -- required
"opt_text" TEXT NOT NULL DEFAULT ''   -- optional
```

So an external consumer that needs the distinction must test *`NOT NULL` **and
no default***. Testing `NOT NULL` alone reads every optional column as required.

This is worth stating plainly because getting it wrong is expensive and silent.
A migration tool that read `NOT NULL` as "this reference must exist", and
dropped rows whose parent was missing, discarded every storefront order in a
production database — the `order_id` column it was checking is legitimately
empty until an order is confirmed.

If you can read the API instead of the schema, prefer it: `openapi.json` lists
required fields explicitly in each model's create schema, and it says what it
means rather than implying it.

## Getting a genuinely nullable column

**Declare the field a pointer.** That is the supported way to say "this may have
no value" to the database:

```go
OrderID *string `db:"order_id" mfx:"relation"`
```

A pointer field is emitted `NULL`, gets no synthesised zero default, reads back
as `null` in JSON, and — since the include fix in this release — populates
correctly through `?include=` when it is a `BelongsTo` foreign key. Rows whose
pointer key is genuinely `NULL` come back without the relation key at all.

Use a pointer when the difference between "not set" and "set to the zero value"
matters — an unset price versus a price of zero, an unconfirmed order versus one
attached to order `""`. Use a plain value when it does not.

## Why the columns are not simply nullable

The obvious alternative — emit `NULL` for every non-required scalar — is not
something this version can do safely. **AutoMigrate never rewrites an existing
column**; it creates tables and adds columns, and warns about drift rather than
altering. Changing the emitted nullability would therefore apply only to newly
created tables, leaving the same model with two different schemas depending on
when its table was first created, and no mechanism to reconcile them. That needs
a migration story before it needs a DDL change.

The zero-value default is not decorative either: it is what lets
`ALTER TABLE ADD COLUMN` succeed against a table that already has rows, on both
SQLite and Postgres. A model can grow a field without manual DDL because of it.

## Referential integrity is an application invariant

One more thing the schema will not tell you: a `REFERENCES` constraint is
emitted only for a `BelongsTo` relation carrying an `on_delete` action the
database can enforce. Specifically, a column gets **no** constraint when it is:

- a plain indexed id column with no `mfx:"relation"` — the usual shape for an
  `owner_id`;
- a relation left at the default `on_delete` (a bare `mfx:"relation"`), since
  there is no action to enforce. The exception is a `JunctionModel`, whose keys
  cascade unless the model says otherwise;
- an edge whose deletion maniflex handles itself, which is where soft delete
  lands — those are enforced in the delete path instead, so the set of
  constraints emitted and the set of edges the cascade skips are drawn by the
  same line.

The database will therefore accept an orphan. Referential integrity in a
maniflex application is maintained by the application, not the storage layer, so
a `PRAGMA foreign_key_check` or a Postgres constraint scan proves less about a
maniflex database than it looks like it does.

## Out of contract for v1

- Altering an existing column's type or nullability during migration.
- Expressing required-ness in the schema in any way other than the absent
  DEFAULT described above.
- `CHECK` constraints derived from `mfx` validation tags (`min:`, `max:`,
  `enum:`); those are enforced in the Validate step, not the database.
