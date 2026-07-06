# Changelog

## v0.1.3 (2026-07-06)

- `events.DeliverWithRetry` no longer swallows handler failures silently: it logs a WARN on each retried attempt and an ERROR when all attempts are exhausted (dead-lettering when a DLQ is configured, or "event dropped" when it isn't). Previously a handler that failed every time produced no log at all, so failing events vanished without a trace.
- `maniflex.List[T]` no longer returns zero rows when passed a non-nil `*QueryParams` whose `Limit` is left at its zero value. A hand-built `&QueryParams{Filters: …}` previously issued `LIMIT 0`; `Limit <= 0` (and `Page <= 0`) now fall back to the default, matching the `q == nil` case. Only the programmatic typed `List` was affected — the HTTP list path fills the limit from `?limit`.
- `GET /health` with `HealthCheckDB: true` now reports `db:"ok"` (or `"error"`) even before any model is registered. The ping set previously came only from registered models, so a bootstrapping server with a reachable `Config.DB` but no models yet reported `db:"unknown"`. `Config.DB` is now always included.
- Raw SQL (`ctx.RawQuery`/`ctx.RawExec` and the adapter's `Raw`) now rebinds `?` placeholders to the adapter's dialect, so `?` works on Postgres (`$1, $2, …`) as well as SQLite. `?` inside string literals is left untouched.
- `ctx.RawQuery` now returns the rows from a data-modifying statement with a `RETURNING` clause (`UPDATE … RETURNING`, `INSERT … RETURNING`), on both the autocommit and in-transaction paths. Previously query-vs-exec was decided by a `SELECT`-prefix check, so a `RETURNING` statement was routed to `ExecContext` and its result set was silently discarded.
- `mfx:"unique"` is now honoured when the column is added to an existing table via `ALTER TABLE ADD COLUMN`, not only when the table is first created. AutoMigrate creates a `UNIQUE INDEX` (`uidx_<table>_<column>`) on both paths. Previously the constraint was silently skipped (warn only) on the add-column path, so a uniqueness guarantee could vanish on a redeploy that added the column. **Behaviour change:** if existing rows already violate the new constraint, migration now fails with an error naming the table and column instead of skipping — resolve duplicate data before deploying.

## v0.1.2 (2026-06-20)

- Docs: corrected the `postgres.Open` examples to the real positional signature `Open(writeDSN, readDSN, registry)` (pool/session tuning via `OpenWithConfig`); documented custom-action behaviours (empty `ctx.Files`, the SQLite single-writer transaction caveat, streaming raw bytes via `ctx.Writer`, emitting events from an action), the `mfx:"file"` pre-uploaded-key existence check, and searching localized (`LocaleString`) content.
- Added `ModelConfig.Headless`: registers a model fully (migration, registry, typed access, relations) but mounts no REST routes, freeing its table path for a custom `server.Action` without the chi route-collision panic. Headless models are also excluded from the generated OpenAPI paths (their schema is still emitted).
- Convention foreign keys (`<Name>ID` fields auto-promoted to a `BelongsTo`) now log a warning at startup when the target model was never registered — the common case of storing a foreign id by design in a microservice. Tag the field `mfx:"norelation"` to silence it. The relation is still created; this only warns.
- Omitting a JSON / `SQLTyper` column (`LocaleString`, JSON maps/arrays, `money.Amount`, …) on create no longer fails with a `NOT NULL` violation: the migrator now gives such columns a synthesised empty-container default (`{}` / `[]` / the type's `driver.Valuer` zero, e.g. `0.0000`). Set `mfx:"required"` to opt a column out of the default.
- Database `NOT NULL` violations now return `422 VALIDATION_ERROR` (missing required field) instead of an opaque `500` or a misleading `409`. `maniflex.ErrConstraint` gained a `Kind` field (`unique` / `foreign_key` / `not_null`) and the SQLite/Postgres error normalisers classify accordingly.
- Removed the documented-but-nonexistent `auth.AllowPublicWrite()` from the docs. Public sign-up is achieved by scoping `JWTAuth` away from the operation (auth scoping is inclusion-only), which the auth tutorial, FAQ, and reference now show.
- Create and update responses now serialise JSON / `SQLTyper` columns (`LocaleString`, JSON-backed `sql.Scanner` types) as objects/arrays, matching the read/list responses, instead of returning them as raw JSON strings. Previously the create/update echo went through the map write path, which carried these columns as the stored text; reads already returned structured values, so the two disagreed.
- Fixed `Register`/`MustRegister` panicking ("model must embed BaseModel") when a `ModelConfig` is inlined inside a slice argument. The `ModelConfig`-binds-to-the-preceding-model rule now applies inside flattened slices too, so the documented `MustRegister(domainA.Models(), domainB.Models())` layout — where each `Models() []any` carries its own inline `ModelConfig`s — works as intended.
- Fixed a silent OR-vs-AND footgun in programmatically built filters: a `maniflex.FilterExpr` left at its zero value (`Group == 0`) is now treated as ungrouped and AND-ed, instead of being OR-ed into "group 0". Multiple hand-built filters (e.g. an ownership-scoped `user_id` + `archived` list) now AND together as intended rather than silently matching extra rows. Internally, OR groups are `Group >= 1` and the URL `?filter[N]=` syntax maps onto `Group N+1`, so the HTTP contract (`?filter[0]=a&filter[0]=b` = OR) is unchanged. Code that built OR groups by hand using `Group: 0` must switch to a positive group index.
- `AutoMigrate` now fails fast with a clear error when a model declares a field whose Go type has no SQL column mapping (e.g. a bare `map[string]any` or `[]string`). Previously such columns were silently dropped, so the model registered and "migrated" successfully while the field was never persisted. Wrap the field in a named type implementing `maniflex.SQLTyper`, or exclude it with the `mfx:"-"` tag.

## v0.1.1 (2026-06-16)

- Fixed the server failing to start when no models are registered; the minimal app now boots and serves `/health` and an empty OpenAPI document
- `db/sqlite` and `db/postgres` now resolve `db/sqlcore` through the core module instead of a separate, non-reproducible `db/sqlcore` dependency
- Corrected Go import paths throughout the documentation to use the full `github.com/xaleel/maniflex` module prefix
- Documented that `server.Handler()` does not run AutoMigrate()

## v0.1.0 (2026-06-15)

- Release of the mostly-stable v0.1.0
- `admin` package remains experimental
