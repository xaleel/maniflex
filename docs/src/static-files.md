# Static Files

Alongside the generated model API, maniflex can serve a directory of plain static
files — HTML, CSS, JavaScript, images, downloads — straight off disk. This is
useful for a small admin page, a landing page, or assets referenced by an
OpenAPI viewer, without standing up a separate web server.

## How it works

On startup the server looks for a directory named **`static/`** in the process
working directory. If it exists, every file inside is served under the
**`/static`** URL path:

```
myapp/
├── main.go
└── static/
    ├── index.html        →  GET /static/index.html
    ├── css/app.css       →  GET /static/css/app.css
    └── logo.png          →  GET /static/logo.png
```

By default there is nothing to configure and nothing to register — drop files in
`static/` and they are served. If the directory does not exist, the server logs
a warning and simply skips mounting it; the rest of the API is unaffected.

## Customising the directory and prefix

Three `maniflex.Config` fields override the defaults:

```go
server := maniflex.New(maniflex.Config{
    StaticDir:    "public",   // serve ./public instead of ./static
    StaticPrefix: "/assets",  // under /assets instead of /static
})
```

| Field            | Default          | Effect                                                            |
| ---------------- | ---------------- | ----------------------------------------------------------------- |
| `StaticDir`      | `<cwd>/static`   | filesystem directory served. A relative path resolves against cwd |
| `StaticPrefix`   | `/static`        | URL prefix the directory is mounted under (at the router root)    |
| `StaticDisabled` | `false`          | set `true` to turn static serving off entirely                    |

`StaticDisabled` is for when a `static/` (or `StaticDir`) directory exists for
other reasons — a build artifact, an embedded asset source — and must not be
exposed over HTTP. A missing directory is still skipped with a warning, so you
only need `StaticDisabled` to suppress an existing one.

## The `/static` route

A few details follow from how the route is mounted (the `buildRouter` block in
`router.go`):

- **Resolved from the working directory.** The default path checked is
  `<cwd>/static`, where `<cwd>` is wherever the process was started — not the
  location of the binary. Run the server from the project root, or `cd` there
  first, so `static/` is found. A `StaticDir` relative path resolves the same way.
- **Mounted outside `PathPrefix`.** Static files live at `/static/...` (or your
  `StaticPrefix`), *not* `/api/static/...`. The `PathPrefix` from `maniflex.Config`
  scopes only the model API and `/openapi.json`; the static mount sits at the
  router root.
- **Trailing-slash redirect.** A request to `/static` (no trailing slash) is
  `301`-redirected to `/static/`. Requests below it are served directly.
- **Directory listing.** Because it is backed by Go's `http.FileServer`, a
  request for a directory with no `index.html` returns a file listing. Add an
  `index.html` to each directory you do not want browsable.

## Choosing the directory

By default the directory is `static/` in the working directory; set `StaticDir`
to serve assets that live elsewhere (no symlink or `cd` gymnastics required). The
static mount is also purely optional: omit the directory entirely — or set
`StaticDisabled: true` — and maniflex serves only the API.

## Static files vs. file uploads

Static serving is for assets *you* ship with the app. It is unrelated to the
file-upload feature, which stores user-submitted files and is wired up
separately through `Config.FileStorage` and the `/files` endpoints. For
user uploads see [File Fields & Uploads](files.md).

| | Static files | File uploads |
|---|---|---|
| URL | `/static/*` | `/files/*` |
| Source | a `static/` directory you commit | user `POST`s at runtime |
| Configured by | convention, or `Config.Static*` | `Config.FileStorage` |
| Use for | app assets, admin pages | avatars, attachments |
