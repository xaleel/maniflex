# Admin Panel

`maniflex/admin` is an opt-in satellite module that mounts a server-rendered
administration panel on top of any maniflex server. It introspects the model
registry to build its navigation and views, and reads/writes data by issuing
**in-process HTTP requests** against the server's own REST API — so every
operation travels the full auth/validate/pipeline stack. The admin never
touches the database directly.

## Adding the module

```bash
go get github.com/xaleel/maniflex/admin
```

Because it is a satellite, importing it is the only thing needed to bring the
admin into your binary. The core `maniflex` module has no dependency on it.

## Quick start

```go
package main

import (
    "net/http"

    "github.com/xaleel/maniflex"
    "github.com/xaleel/maniflex/admin"
    "github.com/xaleel/maniflex/db/sqlite"
    "github.com/go-chi/chi/v5"
)

func main() {
    server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
    server.MustRegister(User{}, Post{}, Comment{})

    db, _ := sqlite.Open(":memory:", server.Registry())
    server.SetDB(db)

    adminHandler := admin.Mount(server, admin.Config{
        Title:                "My App Admin",
        AllowUnauthenticated: true, // local dev only
    })

    r := chi.NewRouter()
    maniflex.Mount(r, server)
    r.Mount("/admin", http.StripPrefix("/admin", adminHandler))
    http.ListenAndServe(":8080", r)
}
```

`Mount` must be called **after** all models are registered and the DB adapter
is set, and **before** the server starts handling requests. It panics early if
neither `Config.Auth` nor `Config.AllowUnauthenticated` is set, so an
unprotected panel can never be shipped by accident.

## Config reference

| Field                  | Type                              | Default            | Description                                                                            |
| ---------------------- | --------------------------------- | ------------------ | -------------------------------------------------------------------------------------- |
| `PathPrefix`           | `string`                          | `"/admin"`         | Mount path; the returned handler serves routes under this prefix                       |
| `Title`                | `string`                          | `"github.com/xaleel/maniflex admin"` | Displayed in the panel header                                                          |
| `Auth`                 | `func(http.Handler) http.Handler` | —                  | Wraps the whole panel with an auth gate; required unless `AllowUnauthenticated` is set |
| `AllowUnauthenticated` | `bool`                            | `false`            | Skips the auth requirement; **local dev only**                                         |
| `Models`               | `[]string`                        | (all)              | Struct names to show; empty means every registered model                               |
| `ReadOnly`             | `bool`                            | `false`            | Hides create/edit/delete UI and unmounts those routes                                  |
| `Templates`            | `fs.FS`                           | —                  | Override FS for custom templates (see [Templates](#templates))                         |
| `StaticFS`             | `fs.FS`                           | —                  | Replaces the embedded CSS/asset bundle                                                 |

## Authentication

Set `Config.Auth` to any `func(http.Handler) http.Handler` middleware — for
example the JWT middleware from `maniflex/middleware/auth`:

```go
import "github.com/xaleel/maniflex/middleware/auth"

adminHandler := admin.Mount(server, admin.Config{
    Title: "My Admin",
    Auth:  auth.JWTAuth(secret, auth.JWTOptions{}),
})
```

The `Auth` wrapper runs before every panel request. Because data reads and
writes go through the API pipeline, the upstream `Auth` middleware registered
on your model endpoints also enforces field-level and operation-level rules —
the admin doesn't bypass them.

> **Never** set `AllowUnauthenticated: true` in a production deployment.
> Mount panics at startup if neither option is provided, so there is no way to
> accidentally omit auth and only discover it at runtime.

## Views

### Dashboard

`GET /admin/` — one summary card per visible model showing its total row
count, fetched in-process from the API.

### List

`GET /admin/{model}` — a paginated table of records.

- **Pagination** — 20 rows per page; `?page=N` navigates.
- **Sorting** — a dropdown built from fields tagged `mfx:"sortable"`. The
  current direction is preserved across filter changes.
- **Filtering** — one input per field tagged `mfx:"filterable"`. Enum fields
  render a `<select>`; other fields render a text input. Active filters persist
  in the URL as `?f_<field>=<value>`.

### Detail

`GET /admin/{model}/{id}` — all readable fields for one record.

- FK fields rendered as links to the related record (`/admin/{related}/{fk_id}`).
- `HasMany` relations shown as "View {related}" links (list pre-filtered to this
  record's ID).
- Edit and Delete actions, each protected by a CSRF token.

### Create form

`GET /admin/{model}/new` — an empty form; `POST /admin/{model}` submits it.

### Edit form

`GET /admin/{model}/{id}/edit` — the form pre-filled from the existing record;
`POST /admin/{model}/{id}` submits it.

Both forms share the same template and widget logic:

| Widget     | When used                                                                                   |
| ---------- | ------------------------------------------------------------------------------------------- |
| `text`     | default string fields                                                                       |
| `textarea` | long-text / `text` DB type                                                                  |
| `number`   | integer and float fields                                                                    |
| `checkbox` | boolean fields                                                                              |
| `select`   | fields with `mfx:"enum:…"`                                                                  |
| `relation` | BelongsTo FK fields — a `<select>` populated from the target model                          |
| `file`     | fields tagged `mfx:"file"` — includes a preview/download link when a file is already stored |
| `datetime` | `time.Time` fields — rendered as an `<input type="datetime-local">`                         |

Fields tagged `mfx:"hidden"` or `mfx:"writeonly"` are excluded from the list
and detail views. Fields tagged `mfx:"readonly"` appear on the edit form as
disabled inputs (they are server-managed). `mfx:"immutable"` fields are
editable on create but disabled on edit.

### Delete

`POST /admin/{model}/{id}/delete` — deletes the record via the API and
redirects to the list. Requires a valid CSRF token (present on the detail
page's Delete button).

## CSRF protection

The panel uses **double-submit cookies**. On first form load a random 32-byte
hex token is written to a `_csrf` cookie and embedded in a hidden form field.
Every mutating `POST` verifies that both match before forwarding to the API.
There is nothing to configure — it is always on.

## Model whitelist

To show only a subset of registered models:

```go
admin.Mount(server, admin.Config{
    Models: []string{"User", "Post"},
    // "Comment" will not appear in the panel
})
```

Model names are **Go struct names**, not table names. Models omitted from the
whitelist are hidden from navigation, list, and detail views — they are still
served by the API.

## Read-only mode

```go
admin.Mount(server, admin.Config{
    ReadOnly: true,
})
```

In read-only mode the create/edit/delete routes are not mounted at all, and the
corresponding controls are hidden in the UI. Useful for support teams that need
visibility without write access.

## Templates

Drop in a replacement for any individual template by providing a `fs.FS` on
`Config.Templates`. Any file **not** present in the override FS falls back to
the embedded default. The template file names are:

| File             | View                                     |
| ---------------- | ---------------------------------------- |
| `layout.html`    | outer chrome (header, sidebar, `<head>`) |
| `dashboard.html` | model summary cards                      |
| `list.html`      | paginated table                          |
| `detail.html`    | single-record field list                 |
| `form.html`      | shared create/edit form                  |
| `error.html`     | error page                               |

Example — override only the layout to inject custom branding:

```go
//go:embed templates
var myTemplates embed.FS

admin.Mount(server, admin.Config{
    Templates: myTemplates,
})
```

The templates receive the `viewData` struct. Consult the `admin` package source
(`view.go`) for the full shape of each page's data.

## Static assets

The embedded asset bundle is served under `{PathPrefix}/static/`. To replace
it entirely with a custom CSS file:

```go
//go:embed assets
var myAssets embed.FS

admin.Mount(server, admin.Config{
    StaticFS: myAssets,
})
```

`StaticFS` replaces the whole bundle — include any assets the templates
reference (or adjust the templates to match).

## How it works

The panel is self-contained: it holds a reference to `server.Handler()` and
issues normal `http.Request` objects against it in-process. There is no
separate HTTP round-trip.

```
browser → GET /admin/users
          → admin handler
            → apiClient.list(r, "users", "limit=20&sort=…")
              → server.Handler().ServeHTTP(rw, r')   // in-process
                → full pipeline (Auth → Validate → DB → Response)
            ← []map[string]any
          ← rendered list.html
```

This means:

- Pipeline middleware on the model (tenant isolation, field redaction, soft-
  delete visibility) is enforced on every admin read and write.
- Auth cookies or tokens present on the browser request are forwarded
  unchanged to the API, so per-user permission checks work automatically.
- The admin has no SQL access of its own and cannot bypass business rules.

→ [Satellite Modules](modules.md)  
→ [Field Tags Reference](../defining-your-api/tags.md)  
→ [File Fields & Uploads](../defining-your-api/files.md)  
→ [Pipeline Overview](../the-request-pipeline/pipeline.md)
