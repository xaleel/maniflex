# Configurable BaseModel field tags

Date: 2026-07-31
Status: approved, not yet planned

## Problem

`BaseModel` hard-codes the mfx options on the three columns it contributes:

```go
type BaseModel struct {
	ID        string    `json:"id"         db:"id"`
	CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly,filterable,sortable"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly,sortable"`
}
```

None of it is configurable. A model cannot filter or sort on `id`, and cannot
opt *out* of the query surface on `created_at` / `updated_at` that it never
wanted. The tags are on a framework-owned struct, so there is nowhere to say
otherwise.

Two things are wrong with the current shape, and this spec addresses both:

1. **No configurability.** `?filter=id:in:a,b,c` is a routine need and is
   simply not expressible.
2. **Wrong default.** `filterable` and `sortable` widen a model's public query
   surface. Defaulting them on means every registered model exposes indexable
   query paths over two timestamp columns whether or not it wanted them.

## Design

### New defaults

`BaseModel` becomes uniformly `readonly` and nothing else:

```go
type BaseModel struct {
	ID        string    `json:"id"         db:"id"         mfx:"readonly"`
	CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly"`

	// ... unexported framework-internal carriers, unchanged
}
```

`ID` gains `readonly`, which it did not have. This is deliberate and is a
behavior change: `buildInsertSQL` (`db/sqlcore/write.go:59`) generates a UUID
only when the bound field is empty, so today a client-supplied `"id"` in a
create body is accepted and used. With `readonly`, `steps.go:444` strips it
from the request before binding and the framework always generates the ID.
Several call sites already special-case `id` by hardcoded name alongside
`Tags.Readonly` (`typed_crud.go:129`, `admin/handler.go:522`,
`admin/handler.go:609`); this makes the tag agree with them.

### Configuration surface

One new field on `ModelConfig`:

```go
// BaseModelTags widens the mfx options on the three columns BaseModel
// contributes. Keys are DB column names ("id", "created_at", "updated_at");
// values use the same comma-separated syntax as an mfx struct tag.
//
// Options are unioned onto the built-in mfx:"readonly", so this can widen a
// BaseModel column and can never strip a protective default. Each column
// accepts only the options meaningful for it; anything else is a registration
// error:
//
//	id          filterable, sortable
//	created_at  filterable, sortable, index, hidden
//	updated_at  filterable, sortable, index, hidden
BaseModelTags map[string]string
```

Usage:

```go
server.MustRegister(Post{}, maniflex.ModelConfig{
	BaseModelTags: map[string]string{
		"id":         "filterable,sortable",
		"created_at": "filterable,sortable,index",
	},
})
```

### Why augment-only, with no negation syntax

The value is unioned onto the defaults. There is no way to express
"remove `readonly` from `created_at`".

The alternative — full replace, where the string written *is* the field's
option set — has one failure mode that decides it. `BaseModelTags{"created_at":
"filterable"}` would silently drop `readonly`, making a framework-managed
timestamp client-writable. That is the exact "protective directive silently
absent" shape that `tags_unknown.go` was written to eliminate, and reintroducing
it through a different door is not worth the expressiveness.

Negation (`-sortable`) was considered and rejected as unnecessary: every removal
it would enable is already achievable by not adding the option in the first
place, because the defaults are now minimal.

### Why a per-field allowlist, not a shared one

`id` and the timestamps admit different options:

- **`index` on `id`** — `id` is the primary key and already indexed.
  `buildIndices` (`model.go:943`) would emit a redundant `idx_<table>_id` with
  real write-amplification and disk cost. `db/sqlcore/adapter.go:827` and `:652`
  already special-case `id` for exactly this class of reason.
- **`hidden` on `id`** — the admin panel keys its row links on `id`
  (`admin/view.go:188`, `admin/handler.go:501`), and the cursor tiebreaker
  orders by it (`db/sqlcore/cursor.go:26`). Hiding it produces a broken API
  rather than a leaner one.

An allowlist rather than a denylist because an allowlist fails closed: an option
added to `parseFieldTags` later is rejected on BaseModel columns until someone
deliberately admits it. A denylist would silently accept it.

### Resolution

New file `basemodel_tags.go`:

```go
var baseModelTagOptions = map[string][]string{
	"id":         {"filterable", "sortable"},
	"created_at": {"filterable", "sortable", "index", "hidden"},
	"updated_at": {"filterable", "sortable", "index", "hidden"},
}

func (m *ModelMeta) applyBaseModelTags() error
```

It mutates `m.Fields[i].Tags` in place via `FieldByDBName`. That is safe:
`modelIndex` caches slice *positions*, not pointers or values (`model.go:264`),
and this function does not append to `m.Fields`.

Map iteration order is randomised, so the keys are sorted before iterating.
Without that, a model with two invalid entries reports a different error on each
run, which makes the failure hard to fix and impossible to test.

#### Call-site ordering

`applyBaseModelTags` is called from `ScanModel` immediately after the
`FieldByDBName("id") == nil` check (`model.go:776-779`). That position is
load-bearing — every consumer of the flags it sets runs later:

| Consumer | Reads | Currently at |
| --- | --- | --- |
| `rejectHiddenRequired` / `rejectReadonlyRequired` | `Hidden`, `Readonly` | `model.go:799`, `model.go:802` |
| `collectCursorField` | `Sortable` | `model.go:852` |
| `buildIndices` | `Index` | `model.go:875` |

#### Errors

Three registration errors, all using the existing `suggestOpt` / `nearest`
did-you-mean helpers from `tags_unknown.go`:

1. **Unknown key**

   ```
   maniflex: model "Post" BaseModelTags key "createdat" is not a BaseModel
   column (did you mean "created_at"?) — valid keys are "id", "created_at",
   "updated_at"
   ```

2. **Disallowed option** — uses `nearest(opt, allowed)`, so `"filterble"` still
   suggests `"filterable"`.

   ```
   maniflex: model "Post" BaseModelTags["id"] option "file" does not apply to
   the id column — allowed: filterable, sortable
   ```

3. **Missing column** — defensive. The BaseModel embed is mandatory
   (`ScanModel` rejects a model without it at `model.go:709-715`), so a nil
   lookup should be unreachable; it returns an error rather than skipping
   silently.

Empty values and trailing commas are tolerated, matching `parseFieldTags`
(`tags.go:397`): a bare `""` and a trailing `,` are not typos.

Map keys are DB column names. For these three columns the JSON and DB names are
identical, so there is no JSON-name-versus-DB-name question to resolve — unlike
`ModelConfig.CursorField`, which accepts either.

### Cursor pagination stays a two-part declaration

`collectCursorField` (`cursor.go:473`) requires the cursor column to be
`sortable`. With the new defaults, the canonical setup needs both halves:

```go
type Doc struct {
	maniflex.BaseModel `mfx:"cursor_field:created_at"`
}

server.MustRegister(Doc{}, maniflex.ModelConfig{
	BaseModelTags: map[string]string{"created_at": "sortable,index"},
})
```

`cursor_field` does **not** implicitly grant `sortable`. The point of the change
is that a model's query surface is exactly what its config says; an implicit
widening that does not appear at the `BaseModelTags` call site would undo that.
The missing half is a loud registration error, not a silent one, so the cost is
one clear error message the first time.

The same applies to the `ModelConfig.CursorField` spelling — it resolves through
the same `collectCursorField` check, so it needs the matching `BaseModelTags`
entry too.

### Out of scope

`WithDeletedAt` and `WithIsDeleted` keep their current tags
(`readonly,filterable` and `readonly,filterable,default:false`). They are
opt-in embeds rather than mandatory ones, and their `filterable` default is
load-bearing for soft-delete queries.

## Migration

This is a breaking change to every registered model. It needs a version
decision before release; that decision is not part of this spec.

There is deliberately no compatibility flag. A single global switch restoring
the old defaults would mean two default sets to test, document, and support
indefinitely, for a change whose failure modes are already loud and actionable.

### Verified non-issue

There is **no implicit default `ORDER BY created_at`**. Sort clauses are built
only from parsed `?sort=` expressions (`db/sqlcore/adapter.go:2041`,
`db/sqlcore/fts.go:90`), which pass through the `Tags.Sortable` gate. An
unsorted list is unordered today and remains so. Nothing silently reorders.

### Library changes

- `model.go` — three struct-tag lines, plus the `applyBaseModelTags` call site.
- `config.go` — new `BaseModelTags` field.
- `basemodel_tags.go` — new.

### Error-message rewording

"Add `mfx:"filterable"` to the struct tag" is no longer the fix when the column
belongs to `BaseModel` — the struct is framework-owned. These messages must
name `ModelConfig.BaseModelTags` when the referenced column is a BaseModel one:

- `cursor.go:474` — cursor field not sortable
- `filter.go:330` — field not filterable
- `filter.go:351` — locale field not filterable
- `filter.go:386` — nested field not filterable
- `filter.go:433` — nested field not sortable
- `query.go:261` — sort field not sortable

### Registration failures to fix

These currently assert successful registration and will fail:

- `tags_unknown_test.go:256` — the `BaseModel` embed carrying
  `mfx:"cursor_field:created_at"`
- `tests/e2e/cursor_pagination_test.go:29` — `CursorByTime`
- `tests/e2e/fts_test.go:24`

### Query failures to fix

- `tests/e2e/filter_test.go:59` — `?filter=created_at:eq:…`
- `tests/e2e/export_test.go:106` — `?sort=created_at:asc`

### Silent capability loss to audit

`admin/util.go:115 sortOptions` iterates `Tags.Sortable`, so every admin list
view loses its `created_at` sort option until the model opts in. This produces
no error — only a missing dropdown entry. Framework-owned models to audit:

- `jobs/maniflex/mount.go` — `StatusModel`
- `pkg/ledger` — `LedgerEntry`, `LedgerLine`
- `tests/e2e/testutil/models.go` — shared fixtures

### Docs and examples advertising the old behavior

- `README.md:77`
- `docs/src/index.md:23`
- `docs/src/getting-started.md:129`
- `docs/src/example-1.md:155`
- `docs/src/using-the-api/querying.md:81,111,145,228,235,329`
- `docs/src/advanced-topics/export.md:48`
- `docs/src/reference/ai-agents.md:268`
- `docs/src/defining-your-api/models.md:51-64` — reproduces the struct verbatim
- `docs/src/defining-your-api/tags.md`
- `docs/llms.txt`
- `examples/blog.go:131`
- `examples/analytics.go:798`

`CHANGELOG.md` needs an entry.

## Testing

### Unit — `basemodel_tags_test.go`

- Each allowed option on each column sets the corresponding `FieldTags` flag.
- The union preserves `readonly` on all three columns.
- An unknown key errors, and the message carries a did-you-mean suggestion.
- A disallowed option errors, and the message lists the allowed set for that
  column.
- Two invalid entries in one map produce a *deterministic* error across repeated
  runs — the test that pins the sorted-keys requirement.
- An empty value (`""`) and a trailing comma (`"filterable,"`) both register
  cleanly.

### Sync test

Mirroring the existing round-trip test in `tags_unknown_test.go`: every option
named in `baseModelTagOptions` must be an option `parseFieldTags` actually
recognises. This prevents the allowlist drifting into naming a directive that no
longer exists.

### Golden defaults test

Scanning a bare model yields exactly `readonly` on all three BaseModel columns
and nothing else. This is the test that fails loudly if a default is
reintroduced later.

### Integration

- `cursor_field:created_at` with `BaseModelTags{"created_at": "sortable"}`
  registers successfully.
- The same model without the `BaseModelTags` entry fails with the reworded
  `collectCursorField` message.

### e2e

- `?sort=created_at:desc` returns 400 on a default model.
- The same request returns 200 once the model opts in via `BaseModelTags`.

### Fixture migration

The three registration-failure sites and two query-failure sites listed under
Migration.
