# Static Files

Alongside the generated model API, maniflex can serve a directory of plain static
files — HTML, CSS, JavaScript, images, downloads — straight off disk. This is
useful for a small admin page, a landing page, or assets referenced by an
OpenAPI viewer, without standing up a separate web server.

## How it works

Static serving is **opt-in**: set `StaticDir` to the directory you want served,
and every file inside it is served under the **`/static`** URL path.

```go
server := maniflex.New(maniflex.Config{
    StaticDir: "static", // serve ./static — nothing is served unless you name a dir
})
```

```
myapp/
├── main.go
└── static/
    ├── index.html        →  GET /static/index.html
    ├── css/app.css       →  GET /static/css/app.css
    └── logo.png          →  GET /static/logo.png
```

Files are served verbatim, so a single-page app is served in full — its
`index.html` at the directory root and every nested asset at its own path:

```
static/
├── report.json                     →  GET /static/report.json
└── admin/                          →  GET /static/admin/  (serves index.html)
    ├── index.html
    └── scripts/app.js               →  GET /static/admin/scripts/app.js
```

If `StaticDir` is left empty, no static route is mounted and maniflex serves only
the API. If it is set but the directory does not exist, the server logs a warning
and skips the mount; the rest of the API is unaffected.

> **Changed in v0.2.1.** Static serving used to default to `<cwd>/static` — any
> `static/` directory in the working directory was published at `/static/`
> automatically. It is now opt-in: set `StaticDir` explicitly. If you relied on
> the old default, add `StaticDir: "static"`.

## Customising the directory and prefix

Three `maniflex.Config` fields control static serving:

```go
server := maniflex.New(maniflex.Config{
    StaticDir:    "public",   // serve ./public
    StaticPrefix: "/assets",  // under /assets instead of /static
})
```

| Field            | Default   | Effect                                                            |
| ---------------- | --------- | ----------------------------------------------------------------- |
| `StaticDir`      | `""`      | filesystem directory served; empty serves nothing. A relative path resolves against cwd |
| `StaticPrefix`   | `/static` | URL prefix the directory is mounted under (at the router root)    |
| `StaticDisabled` | `false`   | set `true` to turn serving off even when `StaticDir` is set       |

`StaticDisabled` exists so an app that sets `StaticDir` unconditionally can still
flip serving off from an env var or flag without clearing the field.

## The `/static` route

A few details follow from how the route is mounted (the `buildRouter` block in
`router.go`):

- **Resolved from the working directory.** A relative `StaticDir` resolves
  against `<cwd>`, wherever the process was started — not the location of the
  binary. Run the server from the project root, or `cd` there first, so a
  relative path like `"static"` is found.
- **Mounted outside `PathPrefix`.** Static files live at `/static/...` (or your
  `StaticPrefix`), *not* `/api/static/...`. The `PathPrefix` from `maniflex.Config`
  scopes only the model API and `/openapi.json`; the static mount sits at the
  router root.
- **Trailing-slash redirect.** A request to `/static` (no trailing slash) is
  `301`-redirected to `/static/`. Requests below it are served directly.
- **Directory listing.** Because it is backed by Go's `http.FileServer`, a
  request for a directory with no `index.html` returns a file listing. Add an
  `index.html` to each directory you do not want browsable — and only point
  `StaticDir` at a directory whose whole contents are safe to publish.

## Static files vs. file uploads

Static serving is for assets *you* ship with the app. It is unrelated to the
file-upload feature, which stores user-submitted files and is wired up
separately through `Config.FileStorage` and the `/files` endpoints. For
user uploads see [File Fields & Uploads](files.md).

| | Static files | File uploads |
|---|---|---|
| URL | `/static/*` | `/files/*` |
| Source | a directory you commit and name in `StaticDir` | user `POST`s at runtime |
| Configured by | `Config.Static*` | `Config.FileStorage` |
| Use for | app assets, admin pages | avatars, attachments |
