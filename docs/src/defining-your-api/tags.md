# Field Tags Reference

A model's behaviour is declared with three struct tags on each field. This page
documents all three, and every directive accepted by the `mfx` tag.

## The three tags

| Tag    | Controls                                         | Default if omitted              |
| ------ | ------------------------------------------------ | ------------------------------- |
| `json` | field name in request and response bodies        | snake_case of the Go field name |
| `db`   | database column name                             | the resolved `json` name        |
| `mfx`  | field behaviour — validation, querying, and more | no directives                   |

```go
Title string `json:"title" db:"title" mfx:"required,filterable,sortable"`
```

The `mfx` tag holds a comma-separated list of directives. Whitespace around each
directive is trimmed. Directives are either **flags** (a bare word) or
**key-value** directives (`key:value`).

An unrecognised directive is a **registration error**, and the message names the
directive it thinks you meant:

```
maniflex: model "User" field "Role" has unknown mfx option
"read_only" (did you mean "readonly"?) — an unrecognised option is
not applied, so a protective directive that is misspelt leaves the
field unprotected
```

Directives used to be matched exactly and anything else discarded in silence.
For a descriptive directive that was merely puzzling — a misspelt `sortable` just
meant no sorting — but for a protective one it was a hole: `mfx:"read_only"`
left `Readonly` false, so the field stayed writable by any client, with nothing
in the logs or the OpenAPI spec to show it. Directives are case-sensitive and
lowercase; `mfx:"Readonly"` is rejected rather than quietly accepted.

## Excluding a field

A field is dropped from the model entirely — no column, not in any payload — if
any of its three tags is set to `-`:

```go
Internal string `mfx:"-"`     // excluded
Cache    string `json:"-"`    // excluded
Scratch  string `db:"-"`      // excluded
```

## Validation directives

These constrain the values a field accepts on write.

| Directive      | Effect                                                           |
| -------------- | ---------------------------------------------------------------- |
| `required`     | the field must be present in a create request                    |
| `enum:a\|b\|c` | the value must be one of the pipe-separated options              |
| `min:N`        | numeric minimum (`N` is a number)                                |
| `max:N`        | numeric maximum                                                  |
| `default:V`    | value applied when the field is absent; cast to the field's type |

```go
Status   string `json:"status"   mfx:"required,enum:draft|published|archived"`
Priority int    `json:"priority" mfx:"min:1,max:5,default:3"`
```

### How `default:` is applied

`default:V` becomes a SQL `DEFAULT` clause on the column. Nothing applies it in
Go — it fires because the `INSERT` omits the column, so it takes effect whenever
a write does not name the field. That holds for both doors into a model:

| Create path | Column omitted when |
|---|---|
| `POST /:model` | the request body has no such key |
| `maniflex.Create[T]` | the struct field is at its Go zero value |

The Go zero is the only signal a struct can give, so a defaulted column cannot
be given an explicit zero through `Create[T]` — `Priority: 0` against
`default:3` stores 3. Make the field a pointer when that distinction matters:

```go
Priority *int `json:"priority" mfx:"default:3"` // nil → 3, new(int) → 0
```

Only defaulted columns behave this way. A zero on a field with no `default:`
tag is written as that zero, on both paths.

> Before v0.2.5, `Create[T]` wrote every column at its Go value and a `default:`
> never fired — the same model created over HTTP and in Go disagreed. In the
> same release, `default:` on a pointer field started reaching the schema at all;
> it was previously emitted only for `NOT NULL` columns and silently dropped.

## Write-access directives

These govern whether a field can be set by a client, and when.

| Directive   | Effect                                                                  |
| ----------- | ----------------------------------------------------------------------- |
| `readonly`  | stripped from all write operations; values sent by a client are ignored |
| `immutable` | accepted on create, rejected on update                                  |

`BaseModel`'s `created_at` and `updated_at` are `readonly`. Use `immutable` for
values that are set once and must not change afterwards, such as an owner ID.

```go
Email   string `json:"email"    mfx:"required,immutable"`
ApiKey  string `json:"api_key"  mfx:"readonly"`
```

## Response-visibility directives

Both directives drop the field from API responses. They differ in whether the
client may _write_ the field.

| Directive   | Read in responses | Write on create / update |
| ----------- | ----------------- | ------------------------ |
| `writeonly` | no                | **yes**                  |
| `hidden`    | no                | no                       |

- **`writeonly`** is for values the client must supply but should never see
  again — typically passwords. The field is included in the create and update
  request schemas; only the response is scrubbed.
- **`hidden`** is for values clients have no business touching at all —
  server-managed internals, audit fields, derived data. The field is dropped
  from create and update schemas as well, so it cannot be set from the API. It
  is still stored, and code running inside the pipeline (a middleware on the
  Service step, for example) can populate it.

```go
// Client sets it on create, never sees it back.
Password string `json:"password" mfx:"required,writeonly"`

// Server-managed; client can neither read nor write it.
InternalScore float64 `json:"internal_score" mfx:"hidden"`
```

`hidden` therefore implies `readonly` — writing it out as `mfx:"hidden,readonly"`
is allowed but redundant. If you want a field the client writes but never reads,
that is `writeonly`, and spelling both (`mfx:"hidden,writeonly"`) leaves it
writable: the explicit directive wins over the implication.

`mfx:"hidden,required"` is **rejected at registration**. The two contradict —
`hidden` stops the client sending the field, `required` insists it does — so no
request could satisfy it. A value the client must supply but never reads back is
`mfx:"writeonly,required"`.

A field that is both `readonly` and not `hidden` is the opposite case: visible
in responses, never accepted from the client.

**`json:"-"`** is treated as hidden **and** read-only: the field stays a real,
persisted column that the server owns, but it never appears in API responses and
is never accepted from the client. This matches the Go convention that `json:"-"`
affects serialization, not schema. To exclude a field from the database entirely
(no column), use `db:"-"` or `mfx:"-"` instead.

## Record-locking directives

These freeze a _whole record_ — not just a field — once its state matches a
condition. Useful for terminal states in business workflows (posted invoices,
closed pay periods, confirmed POs).

| Directive               | Effect                                                                                            |
| ----------------------- | ------------------------------------------------------------------------------------------------- |
| `lock_when:field=value` | when the existing record's `field` equals `value`, updates and deletes return 422 `RECORD_LOCKED` |

Multiple `lock_when` directives accumulate; **any** matching condition locks
the record. The directive can be written on any field — the referenced
`field` is what matters. A typo in the referenced JSON name is caught at
registration so you never ship a rule that silently never matches.

```go
type Invoice struct {
    maniflex.BaseModel
    Number string
    Status string `mfx:"enum:draft|posted|void,lock_when:status=posted,lock_when:status=void"`
    Amount int
}
```

The transition into a locked state is _itself_ allowed — when the request
arrives, the loaded record is still in its previous state. After that
update commits, the record becomes frozen.

`lock_when` is checked before the default Validate step's other rules on
update, and before the adapter's Delete call on delete. Creates are
exempt: there is no prior state to check.

The guard fails closed. It reads the record through the request's transaction
when one is active — so it sees state the same request has written but not yet
committed — and if that read fails for any reason other than "no such record",
the request is rejected (500 `DB_ERROR`) rather than allowed through unchecked.

## Pessimistic lock directive

| Directive               | Effect                                                                                                  |
| ----------------------- | ------------------------------------------------------------------------------------------------------- |
| `lock_scope:ModelName`  | before a create, acquire a `SELECT … FOR UPDATE` lock on the row referenced by this field's value      |

Eliminates manual `ctx.LockForUpdate` calls in the most common case: a
create that must read-then-write a shared resource without a concurrent
write sneaking in between.

```go
type Dispense struct {
    maniflex.BaseModel
    StockID  string `json:"stock_id"  db:"stock_id"  mfx:"required,lock_scope:StockBalance"`
    Quantity int    `json:"quantity"  db:"quantity"  mfx:"required,min:1"`
}
```

**Requirements:**

- The model must run inside a transaction. Register `maniflex.WithTransaction(nil)`
  on the Service step; otherwise the DB step aborts with `500 LOCK_SCOPE_NO_TX`.
- The referenced model name must be registered. A typo is caught at startup
  (in `Handler()`), so it never reaches production silently.
- If the referenced row does not exist, the create returns `404 NOT_FOUND`.

```go
server.Pipeline.Service.Register(
    maniflex.WithTransaction(nil),
    maniflex.ForModel("Dispense"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

**Comparison with `ctx.LockForUpdate`:**

| | `lock_scope` tag | `ctx.LockForUpdate` |
|---|---|---|
| Declaration | struct tag | custom Service middleware |
| Fields locked | one per tag directive | any ID at runtime |
| Requires transaction | yes (enforced at runtime) | yes (enforced at call time) |
| Use when | one fixed FK to lock | dynamic or multiple targets |

See [Transactions](../the-request-pipeline/transactions.md) for the underlying `ctx.LockForUpdate`
and `BeginTx` APIs.

## Query directives

These opt a field into the query string. A field is not filterable or sortable
unless explicitly tagged.

| Directive             | Effect                                                                                   |
| --------------------- | ---------------------------------------------------------------------------------------- |
| `filterable`          | the field may be used in `?filter=`                                                       |
| `sortable`            | the field may be used in `?sort=`                                                         |
| `searchable`          | the field is indexed for native full-text search (`?q=`); text columns only              |
| `cursor_field:<name>` | opt the model into keyset (cursor) pagination; `<name>` is the column to walk by — it must be `sortable` and non-nullable (a pointer field is rejected at registration) |

See [Querying](../using-the-api/querying.md) for the filter, sort, and cursor-pagination grammar.

## Schema directives

| Directive | Effect                                                                            |
| --------- | --------------------------------------------------------------------------------- |
| `unique`  | a hint to the adapter to add a `UNIQUE` constraint on the column                   |
| `index`   | create a (non-unique) index on the column during AutoMigrate                       |

```go
Slug  string `json:"slug"  mfx:"required,unique"`
Email string `json:"email" mfx:"index"`
```

`index` creates an index named `idx_<table>_<column>`. It is skipped when the
column is already covered by another index — a `unique` constraint on the same
field (databases index unique columns implicitly), a `ModelConfig.Indices` entry,
or a scheduled-column auto-index — so adding it is always safe. Indexing a
foreign-key column (e.g. `mfx:"index"` on `UserID`) is a common, valid use.

`unique` is enforced on both the create-table and the add-column (`ALTER TABLE`)
paths — AutoMigrate creates a `UNIQUE INDEX` named `uidx_<table>_<column>` in both
cases. Adding a `unique` column to a table that already holds rows with duplicate
values in that column **fails migration** (the index build errors, naming the
table and column) rather than silently dropping the constraint; resolve the
duplicates before deploying.

### JSON and other custom columns

A bare `map[string]any`, `map[string]string`, or `[]string` field has **no SQL
column mapping** and fails `AutoMigrate` with a clear error. Wrap it in a named
type that implements `maniflex.SQLTyper` (plus `driver.Valuer` + `sql.Scanner`)
so it controls its own column type — e.g. a `JSONMap` that maps to `JSONB` on
Postgres and `TEXT` on SQLite. `maniflex.LocaleString` is a built-in example. To
keep such a field out of the database entirely, tag it `mfx:"-"`.

## Relation directives

A field may declare a relationship to another model. Relations are **opt-in** —
an `<Name>ID` field is a plain column unless you tag it.

| Directive                       | Effect                                                                                                 |
| ------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `relation`                      | marks an FK field as a `BelongsTo`; the target is inferred from the field name (`AuthorID` → `Author`) |
| `relation:Name`                 | explicit relation; `Name` is the companion struct field carrying the target type                       |
| `relation:Name;onDelete:action` | sets the referential action — `cascade`, `setNull`, or `restrict`                                      |
| `through:Model`                 | on a slice field, declares a many-to-many relation through the named junction model                    |
| `norelation`                    | **deprecated no-op** — relations are no longer inferred from the `ID` suffix, so nothing to opt out of  |

`onDelete` sub-options are joined to the `relation:` directive with a semicolon,
not a comma. Relationships are covered in full in [Relations](relations.md).

A field whose name ends in `ID` (e.g. `UserID`) is a plain scalar column unless
tagged `mfx:"relation"`. So a value column that merely ends in `ID` — an external
reference, an opaque token — needs no special tag:

```go
ExternalID string `json:"external_id"` // just a string, not a relation
UserID     string `json:"user_id" mfx:"relation"` // → User (opt in)
```

## File upload directives

`file` marks a field as a file-upload field. The column stores the storage key;
multipart form-data is then accepted for create and update on the model.

The field's Go type must be `string` (one key) or `maniflex.FileKeys` (many).
Any other type is a registration error — every rule below is keyed on the column
being a storage key, so on another type they would all be silently skipped.

| Directive           | Effect                                                                                             |
| ------------------- | -------------------------------------------------------------------------------------------------- |
| `file`              | mark the field as a file upload                                                                    |
| `max_size:N`        | maximum file size; accepts `KB`, `MB`, `GB` suffixes, or plain bytes. On `FileKeys`, per file      |
| `max_count:N`       | `FileKeys` only — maximum number of keys (default 100)                                             |
| `accept:p1\|p2`     | allowed MIME-type patterns, e.g. `image/*\|application/pdf`                                        |
| `auto_delete:false` | keep the stored file when the record is hard-deleted or the field is replaced (default: delete it) |
| `upload:presigned`  | mount `POST /{model}/{field}/upload-url` so the client uploads straight to storage                 |
| `file_acl:private`  | (default) response carries the raw storage key                                                     |
| `file_acl:signed`   | response replaces the key with a pre-signed URL (TTL: `Config.FilesConfig.SignedURLTTL`, default 1h)       |
| `file_acl:public`   | response replaces the key with a permanent / long-lived URL                                        |

```go
Avatar string            `json:"avatar" mfx:"file,max_size:2MB,accept:image/*"`
Logo   string            `json:"logo"   mfx:"file,file_acl:public,accept:image/*"`
Images maniflex.FileKeys `json:"images" mfx:"file,accept:image/*,max_count:10"`
```

See [File Fields & Uploads](files.md) for the upload workflow, and
[Many files per field](files.md#many-files-per-field-filekeys) for `FileKeys`.

## Encryption directives

| Directive   | Effect                                                             |
| ----------- | ------------------------------------------------------------------ |
| `encrypted` | the field is encrypted at rest (AES-256-GCM) and decrypted on read |
| `key:name`  | the key name passed to the key provider; defaults to `default`     |

Encrypted fields cannot be filtered or sorted, because the stored value is
ciphertext. If `unique` is also set, a companion `{field}_hmac` column enforces
uniqueness without exposing the plaintext.

```go
SSN string `json:"ssn" mfx:"encrypted,key:patient-pii"`
```

## Scheduled directives

The `scheduled` directive declares a time-driven transition on a timestamp
field — for example, soft-deleting a row once a timestamp passes. The directive
only _marks_ the field; the transitions are applied by a background runner
documented in [Events & Background Jobs](../advanced-topics/events-jobs.md).

It is an advanced feature with several sub-options joined by semicolons:

```go
ExpiresAt time.Time `json:"expires_at" mfx:"scheduled;soft-delete"`
PublishAt time.Time `json:"publish_at" mfx:"scheduled;field=status;from=draft;to=published"`
```

| Sub-option    | Effect                                                   |
| ------------- | -------------------------------------------------------- |
| `soft-delete` | soft-delete the row when the timestamp is reached        |
| `hard-delete` | permanently delete the row when the timestamp is reached |
| `field=F`     | the field to change                                      |
| `from=V`      | apply only when `field` currently equals `V`             |
| `to=V`        | the value to set `field` to                              |

## Locale directives

These apply to fields declared as `maniflex.LocaleString` — a multilingual
string stored as a JSON object keyed by locale code (e.g. `{"en":"Finance","ar":"مالية"}`).

Mark a field as locale-aware with `mfx:"locale"`. All other locale directives
require `locale` to be present.

| Directive             | Effect                                                                                                   |
| --------------------- | -------------------------------------------------------------------------------------------------------- |
| `locale`              | marks the field as a `LocaleString`; enables locale-aware response serialisation                         |
| `split`               | (default) response emits `"name"` = resolved string and `"name_i18n"` = full map                         |
| `resolve`             | response always emits `"name"` as a plain string; no companion field                                     |
| `dynamic`             | response emits a string when `?locale=` is set, the full map otherwise                                   |
| `default_locale:code` | field-level fallback locale (e.g. `default_locale:ar`) when the client did not request a specific locale |

```go
type Department struct {
    maniflex.BaseModel
    Name maniflex.LocaleString `json:"name" mfx:"locale,filterable,sortable"`
    Bio  maniflex.LocaleString `json:"bio"  mfx:"locale,resolve,default_locale:ar"`
}
```

The resolved locale for a request follows a precedence chain:
`?locale=` param → `Accept-Language` header → `default_locale` tag → model
`DefaultLocale` → app `LocaleOptions.Default` → `"en"`.

See [Localization](localization.md) for the full LocaleResolver setup and
filtering/sorting behaviour.

## Quick reference

| Directive                                                    | Category              |
| ------------------------------------------------------------ | --------------------- |
| `required`                                                   | validation            |
| `enum:…` `min:` `max:` `default:`                            | validation            |
| `readonly` `immutable`                                       | write access          |
| `hidden` `writeonly`                                         | response visibility   |
| `filterable` `sortable` `searchable` `cursor_field:…`        | querying              |
| `unique` `index`                                             | schema                |
| `relation` `relation:…` `through:…`                          | relations             |
| `file` `max_size:` `max_count:` `accept:` `auto_delete:false` `file_acl:` `upload:presigned` | file upload |
| `encrypted` `key:…`                                          | encryption            |
| `scheduled;…`                                                | scheduled transitions |
| `locale` `split` `resolve` `dynamic` `default_locale:…`      | localization          |
| `-`                                                          | exclude the field     |
