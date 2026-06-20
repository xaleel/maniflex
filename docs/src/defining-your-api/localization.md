# Localization

maniflex has first-class support for multilingual string fields. A single
`maniflex.LocaleString` field stores all translations in one JSON column and
the framework resolves the right one for each request automatically.

## The `LocaleString` type

`maniflex.LocaleString` is a `map[string]string` where each key is a locale
code and each value is the translation for that locale:

```json
{ "en": "Finance", "ar": "مالية", "fr": "Finance" }
```

On SQLite it is stored as `TEXT` (JSON-encoded). On Postgres it is stored as
`JSONB`, which allows GIN-indexed key lookups.

Declare a field as locale-aware with the `locale` directive:

```go
type Department struct {
    maniflex.BaseModel
    Name maniflex.LocaleString `json:"name" mfx:"locale,filterable,sortable"`
    Code string                `json:"code" mfx:"required,unique"`
}
```

On create and update the client sends the full map:

```json
{ "name": { "en": "Finance", "ar": "مالية" }, "code": "FIN" }
```

On read the framework emits a locale-resolved view (see [Response modes](#response-modes)).

## Response modes

Every `LocaleString` field has a **response mode** that controls the shape of
the field in API responses. The mode is resolved in this order:

1. The field's own `mfx` tag (`split`, `resolve`, or `dynamic`)
2. The model's `ModelConfig.DefaultLocaleMode`
3. The app's `LocaleOptions.DefaultLocaleMode`
4. The framework default: **`split`**

### `split` (default)

Two keys are emitted in the response:

- `name` — the resolved string for the effective locale
- `name_i18n` — the full `map[string]string` (always present)

```json
{
	"name": "Finance",
	"name_i18n": { "en": "Finance", "ar": "مالية" }
}
```

The resolved string gives display code a stable `string` type; the companion
`_i18n` map gives the editor everything it needs to build a translation form.
The `_i18n` field is **read-only** — values sent in the request body under
that key are silently ignored.

The companion suffix defaults to `"_i18n"` and is configurable via
`LocaleOptions.SplitSuffix`.

### `resolve`

The field is always a plain string — the resolved value for the effective locale.
No companion field is emitted.

```json
{ "name": "Finance" }
```

Use `resolve` when clients only ever need one language and the extra `_i18n`
key adds no value.

### `dynamic`

Replicates legacy behaviour:

- When `?locale=` is present: emits a string (resolved for that locale)
- When `?locale=` is absent: emits the full map

The field type is non-deterministic. **Not recommended for new models.**

## Setting up the LocaleResolver

Install the `LocaleResolver` middleware on the Deserialize step **before**
registration, so it runs before the framework's built-in Deserialize:

```go
server.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
    Supported:  []string{"en", "ar", "fr"},
    Default:    "en",
    FromHeader: true,
    RTL:        []string{"ar", "he", "fa", "ur"},
}))
```

`LocaleOptions` fields:

| Field               | Type         | Default     | Purpose                                                                                       |
| ------------------- | ------------ | ----------- | --------------------------------------------------------------------------------------------- |
| `Supported`         | `[]string`   | all locales | Whitelist of accepted locale codes; locales not in this list fall back to `Default`           |
| `Default`           | `string`     | `"en"`      | App-wide fallback locale used when the request carries no recognisable preference             |
| `FromHeader`        | `bool`       | `false`     | Also parse `Accept-Language`; first match in `Supported` wins (quality values are ignored)    |
| `RTL`               | `[]string`   | —           | Locale codes with right-to-left script; matching requests get `"_dir":"rtl"` in response meta |
| `DefaultLocaleMode` | `LocaleMode` | `split`     | App-wide default mode for all `LocaleString` fields                                           |
| `SplitSuffix`       | `string`     | `"_i18n"`   | Companion-field suffix used in split mode                                                     |

## Locale resolution chain

When resolving which string to return, the framework walks this chain (most to
least specific) and returns the first non-empty match:

1. Explicit `?locale=` query parameter
2. `Accept-Language` header (first match in `Supported`), when `FromHeader: true`
3. Field's `default_locale:code` tag
4. Model's `ModelConfig.DefaultLocale`
5. App's `LocaleOptions.Default` (default `"en"`)
6. Any non-empty value in the map (last resort)

```go
// Field-level default: Arabic is preferred for this field even when the
// request does not specify a locale.
Bio maniflex.LocaleString `json:"bio" mfx:"locale,default_locale:ar"`
```

```go
// Model-level default: all locale fields on this model use French by default,
// unless overridden by a field's `default_locale` tag.
server.MustRegister(Article{}, maniflex.ModelConfig{DefaultLocale: "fr"})
```

## Requiring a locale key

Use `validate.RequireLocale` to enforce that specific locale keys are present
and non-empty on create (or update) requests:

```go
server.Pipeline.Validate.Register(
    validate.RequireLocale("name", "en"),
    maniflex.ForModel("Department"),
    maniflex.ForOperation(maniflex.OpCreate),
)
```

A request that omits the `"en"` key, or supplies an empty string for it, is
rejected with HTTP 422 `MISSING_LOCALE`. Pass multiple keys to require several
locales at once:

```go
validate.RequireLocale("name", "en", "ar")
```

## Filtering and sorting locale fields

`LocaleString` fields tagged `filterable` and `sortable` work with the
standard query string grammar. In `split` and `resolve` mode the framework
automatically targets the effective locale's JSON key in the database query.

```
GET /departments?filter=name:ilike:%25fin%25&sort=name:asc
```

In the example above, when the effective locale is `"en"`, the adapter runs:

- **SQLite**: `json_extract("departments"."name", '$.en') LIKE '%fin%'`
- **Postgres**: `"departments"."name"->>'en' ILIKE '%fin%'`

You can also filter a specific locale key explicitly:

```
GET /departments?filter=name.ar:contains:مال
```

In `dynamic` mode without an explicit `?locale=` the filter hits the raw JSON
column, which typically returns no results for plain-string comparisons — this
is intentional: in dynamic mode the field's meaning depends on request context.

## Searching localized content

The `mfx:"searchable"` full-text directive (the `?q=` endpoint) indexes **plain
`string` columns only** — tagging a `LocaleString` field `searchable` fails at
model registration, because full-text search has no single text column to index:

```text
maniflex: model "Department" field "Name" is mfx:"searchable" but its type is
maniflex.LocaleString; full-text search only indexes text (string) columns
```

Two ways to make localized content searchable:

- **Substring filter (simplest).** Tag the `LocaleString` field `filterable` and
  use `?filter=name:ilike:%25term%25`. Because the value is stored as one JSON
  object, an `ilike` against the raw column matches across every locale's text at
  once — no extra schema.
- **Denormalized plaintext column.** Add a plain `string` column (e.g.
  `SearchText`) tagged `searchable`, and populate it on create/update from a
  Service-step middleware that flattens the localized strings. Use this when you
  need true `?q=` full-text ranking rather than substring matching.

## RTL meta

When the resolved locale is in `LocaleOptions.RTL`, every response envelope
gains a `meta` object with `"_dir": "rtl"` — for both list responses
(which already carry pagination in `meta`) and single-record responses
(read / create / update):

```json
{
	"data": {
		"name": "مالية",
		"name_i18n": { "en": "Finance", "ar": "مالية" }
	},
	"meta": { "_dir": "rtl" }
}
```

List responses include pagination alongside the direction flag:

```json
{ "data": [...],
  "meta": { "total": 5, "page": 1, "limit": 20, "pages": 1, "_dir": "rtl" } }
```

Clients can use `meta._dir` to switch text direction without needing to know
which locale is active.

## Model-level mode override

Set a uniform mode for all `LocaleString` fields on a model without tagging
each one individually:

```go
server.MustRegister(LegacyArticle{}, maniflex.ModelConfig{
    DefaultLocaleMode: maniflex.LocaleModeDynamic,
})
```

Field-level tags take precedence over the model setting, which in turn takes
precedence over the app-level `LocaleOptions.DefaultLocaleMode`.

## Full example

```go
type Product struct {
    maniflex.BaseModel
    Name        maniflex.LocaleString `json:"name"        mfx:"locale,filterable,sortable"`
    Description maniflex.LocaleString `json:"description" mfx:"locale,resolve"`
    SKU         string                `json:"sku"         mfx:"required,unique"`
}

func main() {
    server := maniflex.New(maniflex.Config{...})

    server.Pipeline.Deserialize.Register(maniflex.LocaleResolver(maniflex.LocaleOptions{
        Supported:  []string{"en", "ar"},
        Default:    "en",
        FromHeader: true,
        RTL:        []string{"ar"},
    }))

    server.MustRegister(Product{})

    db, _ := sqlite.Open("store.db", server.Registry())
    server.SetDB(db)
    server.Start()
}
```

With this setup:

- `GET /products` returns `name` as the resolved English string plus `name_i18n`
  with all translations; `description` is always a resolved English string.
- `GET /products?locale=ar` resolves both fields to Arabic.
- `POST /products` with `{"name":{"en":"Laptop"},"sku":"LAP-01"}` succeeds.
