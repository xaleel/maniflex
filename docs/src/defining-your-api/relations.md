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
`ID` warns at startup (there's no suffix to strip); a future strict mode will
reject it.

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
directly; the junction rows are hidden from the response.

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

## Quick reference

| Goal | Declaration |
|---|---|
| Belongs to `User` (FK column matches) | `UserID string` |
| Belongs to `User` under a different name | `ManagerID string` with `mfx:"relation:Manager"` + `Manager User` companion |
| Has many `Post` | `Posts []Post` (other side carries `UserID`) |
| Many-to-many via `ProductTag` | `Tags []Tag` with `mfx:"through:ProductTag"`, on both sides |
| Cascade on parent delete | `mfx:"...,onDelete:cascade"` |
| Populate in a response | `?include=user,comments` |
