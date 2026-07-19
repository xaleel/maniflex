# Relations

A relation connects two models through a foreign-key column or a junction
table. Relations are declared on the struct — by a field name, a tag, or a
slice — and are populated on demand via the `?include=` query parameter.

maniflex recognises three kinds:

| Kind | Direction | Declared by |
|---|---|---|
| **BelongsTo** | this row holds the FK | an FK field tagged `mfx:"relation"` (or `mfx:"relation:Name"`) |
| **HasMany** | the other table holds the FK | a slice field of the related type |
| **ManyToMany** | a junction table connects both sides | a slice field with `mfx:"through:Junction"` |

## BelongsTo

Relations are **opt-in**: tag the foreign-key field `mfx:"relation"`. The target
model is inferred from the field name with the trailing `ID` stripped
(`UserID` → `User`).

```go
type Post struct {
    maniflex.BaseModel
    Title  string `json:"title" mfx:"required"`
    UserID string `json:"user_id" mfx:"required,filterable,relation"` // → User
}
```

`UserID` is a foreign key to `User`, keyed `user` (snake-case of the trimmed
field name) — the value used in `?include=`, in nested filters
(`?filter=user.role:eq:admin`), and in nested sorts (`?sort=user.name:asc`).

The FK field is also a regular column. Tag it `filterable` if clients need to
query by it, exactly like any scalar.

> **Note:** an `<Name>ID` field is a plain scalar column **unless** you tag it
> `mfx:"relation"`. A value column that merely ends in `ID` — `ExternalID`,
> `CloudEventID`, a third-party token — stays a plain column with no relation and
> no `?include=` key. (The legacy `mfx:"norelation"` opt-out is now a deprecated
> no-op, kept only so existing models compile.)

When the field name doesn't match the target model, name it explicitly with
`mfx:"relation:Target"`. A bare `mfx:"relation"` on a field that doesn't end in
`ID` **fails at startup**: there's no suffix to strip, so the target would be
inferred from the whole field name — a guess that is almost never a real model.

A relation whose target model is simply never registered is allowed by default
(it may be a plain foreign id that wants no relation tag) and rejected under
[strict mode](../reference/strict-mode.md).

### Including the related row

```bash
curl 'localhost:8080/api/posts?include=user'
```

Each post in the response gains a `user` object populated from the related
table. Multiple includes are comma-separated:

```bash
curl 'localhost:8080/api/posts/<id>?include=user,comments'
```

## BelongsTo (explicit)

When the FK field name does not match the target model — for example, a
`ManagerID` pointing to `User` — declare the relation with `mfx:"relation:Name"`
and add a companion field of the target type:

```go
type Team struct {
    maniflex.BaseModel
    Name      string `json:"name"  mfx:"required"`
    ManagerID string `json:"manager_id" mfx:"required,filterable,relation:Manager"`
    Manager   User   `json:"manager,omitempty"`
}
```

- `ManagerID` is the column that stores the FK.
- `Manager` is the *companion field*: it carries the target type (`User`) so the
  framework can resolve the relation. It is not a column itself — its only role
  is type information for relation scanning.

The relation key here is `manager` (snake-case of `Manager`), so the include
becomes `?include=manager`.

A `relation:Name` directive without a matching companion field fails
registration.

## HasMany

The inverse side: a slice of the related struct, declared on the model that
*does not* hold the FK.

```go
type User struct {
    maniflex.BaseModel
    Name  string `json:"name"`
    Posts []Post `json:"posts,omitempty"` // populated when ?include=posts
}
```

There is no column on `users` for this — `Posts` is purely a relation
declaration. The FK is expected on the related table, named after the parent
model: `Post` is expected to carry `user_id`. (That column comes from `UserID`
on `Post`, as in the BelongsTo example above.)

The relation key for a slice is the snake-case of the field's JSON name, here
`posts`.

```bash
curl 'localhost:8080/api/users/<id>?include=posts'
```

## ManyToMany

A many-to-many relation uses a third *junction* model that carries the two FKs.
Both sides declare a slice with `mfx:"through:JunctionModel"`:

```go
type Product struct {
    maniflex.BaseModel
    Name string `json:"name"`
    Tags []Tag `json:"tags,omitempty" mfx:"through:ProductTag"`
}

type Tag struct {
    maniflex.BaseModel
    Label    string    `json:"label"`
    Products []Product `json:"products,omitempty" mfx:"through:ProductTag"`
}

// The junction model — register it just like any other.
type ProductTag struct {
    maniflex.BaseModel
    ProductID string `json:"product_id" mfx:"required,filterable,relation"`
    TagID     string `json:"tag_id"     mfx:"required,filterable,relation"`
}
```

All three models must be registered. The junction's FK columns are declared as
BelongsTo relations (`mfx:"relation"` on `ProductID` and `TagID`), so the
framework can resolve which side of the join goes where.

`?include=tags` on a product follows the junction and returns the related tags
directly.

### Junction payload (`_through`)

A junction often carries data of its own — a role, a position, a date the link
was made. When it does, each included row gains a `_through` object holding
those columns:

```jsonc
{
  "id": "tag-1",
  "label": "blue",
  "_through": { "position": 2, "linked_at": "2026-07-19T09:00:00Z" }
}
```

The two foreign keys and the junction's `id` are excluded — they say nothing the
response does not already carry.

**The junction is a model like any other, and `_through` honours its tags.** A
column marked `mfx:"hidden"` or `mfx:"writeonly"` is absent, exactly as it would
be in a response from the junction's own endpoint:

```go
type ProductTag struct {
    maniflex.BaseModel
    ProductID string `json:"product_id" mfx:"required,filterable,relation"`
    TagID     string `json:"tag_id"     mfx:"required,filterable,relation"`
    Position  int    `json:"position"`                      // in _through
    InvitedBy string `json:"invited_by" mfx:"hidden"`       // not in _through
}
```

Two further exclusions are worth knowing:

- **`mfx:"encrypted"` columns are omitted**, along with their `_hmac`
  companions. The junction payload has no decryption pass, so the alternative
  would be emitting ciphertext — no use to a client, and it discloses the
  encryption envelope. Put a value you need to read on the related model, not on
  the junction.
- **Columns the junction model does not declare are omitted.** A column present
  in the table but absent from the model is schema drift, and dumping it is what
  used to make this a leak.

> Before v0.2.5 `_through` was copied verbatim from the junction row, so hidden
> and write-only columns surfaced on every include.

### Which models are join tables

A junction can be declared three ways. In order of precedence:

| | How | When to use |
|---|---|---|
| Explicit relation | `mfx:"through:Junction"` on a slice field | you want the relation named on the model |
| Explicit marker | embed `maniflex.JunctionModel` | the junction carries columns of its own |
| Auto-detected | two BelongsTo relations, **no other columns** | a plain link table |

Auto-detection accepts only the unambiguous shape — two foreign keys, an `id`,
timestamps, and nothing else:

```go
type ProductTag struct {           // auto-detected
    maniflex.BaseModel
    ProductID string `json:"product_id" mfx:"relation"`
    TagID     string `json:"tag_id"     mfx:"relation"`
}
```

Add a column of its own and detection stops, because that shape is equally what
an ordinary entity looks like:

```go
type Order struct {                // NOT a junction — nothing is inferred
    maniflex.BaseModel
    CustomerID string `json:"customer_id" mfx:"relation"`
    AddressID  string `json:"address_id"  mfx:"relation"`
    Total      int    `json:"total"`
}
```

> Before v0.2.6 `Order` *was* treated as a join table, so Customer and Address
> silently gained a many-to-many through it. If you relied on detection for a
> junction that carries payload, embed `JunctionModel` — see below.

A junction that carries payload says so:

```go
type Enrollment struct {
    maniflex.BaseModel
    maniflex.JunctionModel
    StudentID string `json:"student_id" mfx:"relation"`
    CourseID  string `json:"course_id"  mfx:"relation"`
    Term      string `json:"term"`      // in _through
}
```

`JunctionModel` is embedded **alongside** `BaseModel`, not instead of it — a
junction is an ordinary model with an `id`. The model must have exactly two
BelongsTo relations to distinct models; anything else is a registration error.

`ModelConfig.DisableAutoJunction` opts a model out of detection entirely, for a
link-shaped model that should not gain the relation.

### Unique links

Junction pairs are **not** unique by default. Declare it when they should be:

```go
type ProductTag struct {
    maniflex.BaseModel
    maniflex.JunctionModel `mfx:"unique"`
    ProductID string `json:"product_id" mfx:"relation"`
    TagID     string `json:"tag_id"     mfx:"relation"`
}
```

This emits a `UNIQUE` index over the two key columns, so a repeated link is
refused with `409`. It also lets an include collapse duplicate pairs: with the
declaration a repeat is corruption, without it a repeat is data.

Off by default because a junction carrying its own attributes may legitimately
repeat a pair — `Enrollment{student_id, course_id, term}` holds one row per
term, and each carries its own `_through` payload. A pure link table almost
always wants the tag.

Adding it to a table that already holds duplicates fails the migration until
they are cleaned up. That is why it is separate from the marker: embedding
`JunctionModel` changes nothing about the schema, so declaring what a model *is*
never risks a migration.

### Junction deletes

A junction's foreign keys default to `ON DELETE CASCADE`, so deleting either
endpoint takes its link rows with it — a link to a row that no longer exists
says nothing. An explicit `mfx:"on_delete:..."` on the column wins, and edges
where either side soft-deletes are handled in maniflex's own delete path rather
than by a database constraint (see below).

## Cascading deletes

A BelongsTo relation may declare what happens to this row when the parent it
points at is deleted, using the `onDelete` sub-option:

```go
AuthorID string `json:"author_id" mfx:"relation:Author;onDelete:cascade"`
```

| Action | Effect on this row when the referenced row is deleted |
|---|---|
| `cascade` | this row is deleted too |
| `setNull` | the FK column is set to `NULL` (the field must be a pointer, so it is nullable — otherwise a registration error) |
| `restrict` | the parent delete is refused with `409 DELETE_RESTRICTED` while this row exists |
| omitted | nothing — the delete leaves this row untouched (its FK dangles) |

The action is validated at startup: it must target a **registered** model, and
`setNull` needs a nullable FK. `cascade` recurses — deleting a row deletes its
children, then their children — and reference cycles are handled.

### How it is enforced (and soft delete)

The action is enforced one of two ways, chosen automatically per relation:

- **Neither side soft-deletes** → a real database `FOREIGN KEY … ON DELETE …`
  constraint carries it, and the database enforces it natively (and enforces
  referential integrity on insert: a row naming a non-existent parent is refused
  with `409`).
- **Either side soft-deletes** → the database cannot help (a soft delete is an
  `UPDATE`, so `ON DELETE` never fires, and a DB cascade can only hard-delete),
  so maniflex enforces it in the delete request's own transaction instead. A
  soft-delete child of a cascaded parent is **soft-deleted identically** — its
  `deleted_at` is set, the row is not removed.

Either way the whole deletion is atomic: a `restrict` that fires rolls back any
`cascade` that ran alongside it.

> **Existing SQLite tables.** New FK constraints are declared when a table is
> **created**. SQLite cannot add a foreign key to a table that already exists, so
> adding `onDelete` to a model with a pre-existing SQLite table does not
> retroactively add the constraint — recreate the table, or rely on the
> soft-delete path, which is enforced in the application layer regardless.
> Postgres adds the constraint on the next migration.

## Soft delete and relations

When the related model uses soft delete, rows whose deleted marker is set are
omitted from `?include=` results — the same filter the framework applies to
list endpoints. See [Soft Delete](soft-delete.md).

## Scoping and `?include=`

A request's **forced filters** — the scope imposed by `db.Tenancy` or
`db.ForceFilter` — are applied to included rows as well as to the primary read,
for every relation kind, wherever the related model carries the filtered column.

This matters because the foreign key is the client's to set. Without it, a
caller who can write an FK (or a many-to-many junction row) puts their own row
inside another tenant's `?include=`, and an attach pulls another tenant's record
into their own response. The framework does not validate junction writes, so the
include is where the scope has to hold.

```go
// Tenancy on both models; the include is scoped by the same filter.
server.Pipeline.DB.Register(
    db.Tenancy("org_id", orgOf),
    maniflex.ForModel("Order", "OrderLine"),
)
```

**What is not covered.** Be precise about this, because the gap is narrow but
real:

- A related model with **no such column** is not scoped. That is the right
  answer for the case that produces it — a shared lookup table (currencies,
  categories, statuses) is not tenant-partitioned and has nothing to scope by —
  but if a model *is* partitioned by something the filter cannot name, its
  includes are unscoped and the FK write is yours to validate.
- **Relation-path filters** (`db.ForceFilterVia`) are skipped: they are written
  against the primary model's relations and mean nothing on the related table.
- The scope applies to the **rows returned**, not to the junction. A row that
  should never have been attached is now invisible rather than absent; clean it
  up at the write.

## Quick reference

| Goal | Declaration |
|---|---|
| Belongs to `User` (FK column matches) | `UserID string` |
| Belongs to `User` under a different name | `ManagerID string` with `mfx:"relation:Manager"` + `Manager User` companion |
| Has many `Post` | `Posts []Post` (other side carries `UserID`) |
| Many-to-many via `ProductTag` | `Tags []Tag` with `mfx:"through:ProductTag"`, on both sides |
| Cascade on parent delete | `mfx:"...,onDelete:cascade"` |
| Populate in a response | `?include=user,comments` |
