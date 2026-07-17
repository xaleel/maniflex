# File Fields & Uploads

A field tagged `mfx:"file"` accepts an uploaded file alongside the model's
JSON. The column stores an opaque *storage key*; the bytes live in a
configured `FileStorage` backend. Standalone upload, download, and delete
endpoints can also be mounted (see [Standalone file endpoints](#standalone-file-endpoints)).

> **Config changed.** File settings now live under a single
> `Config.FilesConfig` struct (`maniflex.FilesConfig`). The old flat
> `Config.FileStorage`, `Config.FileSignedURLTTL`, and `Config.FileMiddleware`
> fields have been removed. The mapping is:
>
> | Old (`Config.…`) | New (`Config.FilesConfig.…`) |
> |---|---|
> | `FileStorage` | `Storage` |
> | `FileSignedURLTTL` | `SignedURLTTL` |
> | `FileMiddleware` | `BeforeMiddlewares` |
> | *(implied by `FileStorage != nil`)* | `MountEndpoints` — **now explicit**, see the [footgun](#mountendpoints-is-explicit) |
>
> New: `KeyGen` (custom storage-key layout) and `AfterMiddlewares`
> (post-handler observation).

## Declaring a file field

Add the `file` directive to a string field:

```go
type Article struct {
    maniflex.BaseModel
    Title string `json:"title" mfx:"required"`
    Cover string `json:"cover" mfx:"file,max_size:2MB,accept:image/*"`
}
```

The column's Go and DB types are `string` — what is stored is the storage key
returned after the upload. The on-disk bytes are managed by the storage
backend; the database row holds only the reference.

Tag sub-options:

| Sub-option | Effect |
|---|---|
| `file` | mark the field as a file upload |
| `max_size:N` | per-field size limit; suffixes `KB`, `MB`, `GB` or plain bytes |
| `accept:p1\|p2` | allowed MIME-type patterns, e.g. `image/*\|application/pdf` |
| `auto_delete:false` | keep the stored file when the row is hard-deleted or the field is replaced (default: delete) |
| `upload:presigned` | mount `POST /{model}/{field}/upload-url` so the client uploads straight to storage instead of through the app — see [Direct-to-storage uploads](#direct-to-storage-uploads-uploadpresigned). Requires a backend that can presign (`storage/s3`; not `LocalStorage`) |
| `file_acl:private` | (default) response carries the raw storage key; downloads go via `/files/<key>` or the per-model attachment route |
| `file_acl:signed` | response replaces the key with a pre-signed URL valid for `Config.FileSignedURLTTL` (default 1h). Requires `FileStorage.URL()` |
| `file_acl:public` | response replaces the key with a permanent / long-lived URL (e.g. S3 7-day max). Pair with public-read ACL on the bucket for true permanence |

`accept` matches the content type the client declared on the multipart part; when
the part declares nothing (or the generic `application/octet-stream`), the type is
sniffed from the first 512 bytes. A declared type is a client-supplied claim, so
treat `accept` as an input filter, not a security boundary — downloads are what
enforce safety, via `X-Content-Type-Options: nosniff` and forced attachment for
anything outside the inline allowlist (see
[Standalone file endpoints](#standalone-file-endpoints)).

Tag detail is in [Field Tags Reference](tags.md#file-upload-directives).

### `file_acl` modes

```go
type Attachment struct {
    maniflex.BaseModel
    Logo   string `mfx:"file,file_acl:public,max_size:1MB,accept:image/*"`
    Resume string `mfx:"file,file_acl:signed,max_size:5MB,accept:application/pdf"`
    Notes  string `mfx:"file"`  // implicit private — raw key in the response
}
```

The rewrite happens in the Response step on create, read, list, and update. A
`null`/empty value passes through unchanged — no fabricated URLs to nothing.

Configure the signed-URL lifetime once on `Config`:

```go
maniflex.New(maniflex.Config{
    FilesConfig: maniflex.FilesConfig{
        SignedURLTTL: 15 * time.Minute, // default: 1h
    },
})
```

`LocalStorage.URL` returns the server-relative `/files/<key>` for both signed
and public modes (no real signing — bring an HMAC layer if you need it).
`S3Storage.URL` uses `awss3.NewPresignClient`; `ttl=0` (public mode) maps to
the AWS 7-day maximum.

## Configuring storage

Uploads require a `FileStorage` implementation, set on
`Config.FilesConfig.Storage`. The framework ships one backend; bring your own
for cloud storage.

```go
import "github.com/xaleel/maniflex/storage"

fs, err := storage.NewLocalStorage("./uploads")
if err != nil {
    log.Fatal(err)
}

server := maniflex.New(maniflex.Config{
    Port: 8080,
    FilesConfig: maniflex.FilesConfig{
        Storage:        fs,
        MountEndpoints: true, // mount POST/GET/DELETE /files — see the footgun below
    },
})
```

`FileStorage` is a small interface — `Store`, `Retrieve`, `Delete`, `Exists`,
`URL` — making S3, R2, GCS, or any other key-value store straightforward to
adapt.

When `Storage` is `nil`, model endpoints still accept JSON, but multipart
uploads and the standalone `/files` routes respond with `501 Not Implemented`.

### `MountEndpoints` is explicit

`MountEndpoints` gates only the **standalone** `/files` routes and defaults to
`false`. Setting `Storage` alone is **not** enough to mount them:

| You set | Multipart on `mfx:"file"` fields | Per-model attachment routes | Standalone `/files` |
|---|---|---|---|
| `Storage` only | ✅ enabled | ✅ mounted | ❌ **not mounted (404)** |
| `Storage` + `MountEndpoints: true` | ✅ enabled | ✅ mounted | ✅ mounted |
| `MountEndpoints: true`, `Storage` nil | ❌ 501 | ❌ 501 | ✅ mounted, returns 501 |

> **Migration footgun.** Before this release a non-nil `FileStorage`
> auto-mounted `/files`. That is no longer implied — if you relied on the
> standalone endpoints, add `MountEndpoints: true`. Model file fields and
> per-model attachment routes remain gated on `Storage` alone and are
> unaffected.

### Upload size limits

A multipart request is capped at **32 MB** in total by default, across every
part. Anything larger is rejected with `413 BODY_TOO_LARGE` while it streams —
before a byte is written to a temp file, let alone to storage. Raise or lower
the ceiling per app:

```go
FilesConfig: maniflex.FilesConfig{
    Storage:         fs,
    MaxUploadBytes:  100 << 20, // total request size (default 32 MB)
    MaxUploadMemory:   8 << 20, // buffered in RAM before spooling (default 32 MB)
}
```

`MaxUploadBytes` bounds the **request**; the per-field `max_size` tag bounds an
individual **attachment** within it. Both apply, and the request ceiling is
checked first — so `max_size:2MB` on a field does not stop a client from sending
a 50 GB body, but `MaxUploadBytes` does.

For a tighter limit on one model, register
[`body.MaxBodySize`](../middleware-catalogue/body.md) on the Deserialize step; it
overrides `MaxUploadBytes` for the models it is scoped to.

### Custom storage keys with `KeyGen`

By default a `POST /files` upload is stored under
`uploads/<uuid>/<sanitised-filename>`. Override `KeyGen` to control the layout —
for example, to shard by tenant read from the request context:

```go
FilesConfig: maniflex.FilesConfig{
    Storage:        fs,
    MountEndpoints: true,
    KeyGen: func(ctx *maniflex.ServerContext, h *multipart.FileHeader) string {
        tenant := ctx.Auth.Claims["tenant"] // populated by a BeforeMiddleware
        return fmt.Sprintf("%s/%s", tenant, h.Filename)
    },
},
```

The returned string is used **verbatim** as the storage key — sanitise any
user-supplied component yourself (the default uses `sanitizeFilename`). `KeyGen`
applies only to the standalone `POST /files` route; per-model attachment keys
are framework-generated.

### S3, R2, MinIO, DigitalOcean Spaces

The satellite module `maniflex/storage/s3` ships a `FileStorage` implementation
backed by the AWS SDK v2. It works against any S3-compatible service.

```go
import "github.com/xaleel/maniflex/storage/s3"

store, err := s3.New(ctx, s3.Config{
    Bucket: "my-app-uploads",
    Region: "us-east-1",
    // Endpoint, UsePathStyle, KeyPrefix, ACL are all optional.
})
if err != nil { log.Fatal(err) }
server.SetStorage(store)
```

Credentials follow the standard AWS resolution chain (env vars, shared
config, IAM instance role, IRSA, ECS task role). Override with
`Config.AWSConfig` when you need a custom credential provider, HTTP client,
or retry policy.

Per-service tips:

| Service | Endpoint | UsePathStyle |
|---|---|---|
| AWS S3 | leave empty | `false` |
| MinIO | `http://localhost:9000` | `true` |
| Cloudflare R2 | `https://<account>.r2.cloudflarestorage.com` | `false` |
| DigitalOcean Spaces | `https://<region>.digitaloceanspaces.com` | `false` |

Use `KeyPrefix` to share one bucket across environments
(`KeyPrefix: "staging/"`) — callers pass logical keys and never see the
prefix. File metadata (filename, size, content type) is stored as native
S3 object metadata so objects remain browsable via the AWS console and
`aws s3 cp` without the maniflex layer.

## How uploads work

A model containing one or more `file` fields accepts **multipart/form-data** on
create and update, in addition to JSON:

- Form fields named the same as JSON fields populate the row's scalar values.
- Form *file* parts named after a `file` field are streamed to storage; the
  resulting key is written to the column.

Conceptually:

```
POST /api/articles
Content-Type: multipart/form-data; boundary=...

--...
Content-Disposition: form-data; name="title"

The First Post
--...
Content-Disposition: form-data; name="cover"; filename="hero.png"
Content-Type: image/png

<bytes>
--...
```

The response is the usual JSON envelope; the `cover` field carries the storage
key the client uses to fetch the file later.

The framework rejects an upload before it reaches storage if it violates the
field's `max_size` or `accept` constraints.

### Sending a pre-uploaded key

A `file` field also accepts a plain string in JSON — the storage key of a file
already uploaded via the standalone endpoint. This is useful when the upload
is decoupled from the record creation (large files uploaded ahead of time,
re-using an existing file, and so on).

The key is **checked against storage** before the write: setting a `file` field to
a string key that does not exist in the configured `FileStorage` is rejected with
`422 FILE_NOT_FOUND`, so a record can never reference a dangling key. (Pass JSON
`null` to clear the field.) In production the key exists because the client
uploaded it first — via the multipart part, a prior `POST /files`, or a
[presigned upload](#direct-to-storage-uploads-uploadpresigned). In tests, seed the
key into the shared storage before referencing it.

The field's `max_size` and `accept` rules apply here too, checked against the
object actually in storage — the same bytes get the same answer whichever way they
arrived. **This is new in v0.2.3:** before it, this path checked only that the key
existed, so uploading out of band and referencing the key was a way past both
rules. See the changelog if you relied on that.

## Direct-to-storage uploads (`upload:presigned`)

By default an upload travels client → app → storage, and the app holds the whole
body while it does: the multipart form is drained before the handler runs, and the
in-memory buffer defaults to the same 32 MB as the body cap, so nothing spools to
disk either. A 60 MB video therefore costs 60 MB of server memory and two hops of
bandwidth to store one object.

Add `upload:presigned` and the bytes go straight to the bucket:

```go
type Post struct {
    maniflex.BaseModel
    Title string `json:"title"`
    Video string `json:"video" mfx:"file,upload:presigned,accept:video/mp4,max_size:60MB"`
}
```

That mounts one extra route:

```
POST /posts/video/upload-url
```

There is **no record id in that path**, deliberately: a create-time file field has
no record yet, so a record-scoped route could not serve one. The same route works
for create and update.

The flow is two phases, and the second one is just an ordinary write:

```
① POST /posts/video/upload-url
   {"filename": "clip.mp4", "content_type": "video/mp4", "size": 41231234}

   → 200 {
       "url":        "https://bucket.s3.amazonaws.com/",
       "method":     "POST",
       "fields":     { "key": "...", "policy": "...", "x-amz-signature": "..." },
       "key":        "uploads/<uuid>/clip.mp4",
       "max_size":   62914560,
       "expires_at": "2026-07-17T13:05:00Z"
     }

② the client POSTs the file straight to `url` as multipart/form-data,
   sending every entry of `fields` first and the file last

③ POST /posts   {"title": "...", "video": "uploads/<uuid>/clip.mp4"}
   → the ordinary create, which verifies the object and stores the key
```

Phase ③ is the completion step, and there is nothing else to call: the record
either names the key or it does not. That is why no pending-upload state exists to
reconcile — if a client uploads and never completes, no record references the
object and nothing is corrupt (the object itself is an orphan; see `auto_delete`).

**The field's rules bind at both ends.** At ① the declared `content_type` and
`size` are checked against `accept` and `max_size`, so a URL is never minted for a
file the field would refuse. The limits are then **pinned into the signature** —
S3's POST policy carries a `content-length-range`, so S3 itself rejects an
oversize body — and at ③ the stored object's real size and type are checked again.
That last check is the one that matters: a signature can only bound what the
backend enforces, and the record is what makes an object real.

**The client never chooses the key.** It is minted server-side through
`FilesConfig.KeyGen` (so a per-tenant prefix scheme covers presigned uploads too)
and returned in the response. A client that could name the key could aim its
upload at another record's object.

**Auth applies.** The mint route runs `Auth → handler → Response`, so whatever
gates the model gates the minting — granting the right to write an object is not
something to leave unauthenticated. Note the operation is
`maniflex.OpPresignUpload`, not `OpCreate`: the mint is not the create and can
precede one by minutes, so scope middleware with `ForOperation(OpPresignUpload)`
if you need it there.

> **Backend support.** Presigning requires a backend that can mint one.
> `storage/s3` can (AWS S3, R2, MinIO, Spaces, …). `LocalStorage` **cannot**, and
> says so: the route answers `501 PRESIGN_UNSUPPORTED` rather than handing back an
> unsigned URL, which would be an open write endpoint rather than a degraded
> presigned one. Use the ordinary multipart upload with `LocalStorage`.

## Standalone file endpoints

When `MountEndpoints` is `true`, three routes are mounted under `PathPrefix`:

| Method | Path | Action |
|---|---|---|
| `POST` | `/files` | upload a single file (multipart, field name `file`) |
| `GET` | `/files/{key...}` | stream the file with its original content type |
| `DELETE` | `/files/{key...}` | remove the file from storage |

`POST /files` returns `201` with
`{"data": {"key": "...", "content_type": "...", "size": ..., "filename": "..."}}`.
The returned `key` is the value to store in a `file`-tagged column.

`GET /files/{key...}` streams the body with `Content-Type` and `Content-Length`
from the stored metadata. For safety it always sends `X-Content-Type-Options:
nosniff` and serves only an allowlist of content types (common images, PDF,
plain text) `inline`; everything else — including `text/html` and
`image/svg+xml` — is sent as a `Content-Disposition: attachment` download so a
stored file cannot execute script on the API origin. Missing keys return `404`.

These endpoints are storage-key-addressed and have **no built-in auth**
when `BeforeMiddlewares` is empty. Set it to wrap the routes with the
same pipeline middleware (e.g. JWT, role checks) that protects your model
endpoints:

```go
maniflex.New(maniflex.Config{
    FilesConfig: maniflex.FilesConfig{
        Storage:        fs,
        MountEndpoints: true,
        BeforeMiddlewares: []maniflex.MiddlewareFunc{
            auth.JWTAuth(secret, auth.JWTOptions{}),
            auth.RequireRole("admin"),
        },
    },
})
```

Each middleware sees a synthesised `ServerContext` (Request, Writer, Ctx,
RequestID, logger — no Model/Operation, since these routes are outside
the model pipeline). Aborting the context short-circuits the request
before the file handler runs. Leaving `BeforeMiddlewares` empty keeps the
pre-fix behaviour for backward compatibility, but production deployments
should populate it — anyone who guesses a key could otherwise delete
arbitrary files. The server logs a warning at startup when `/files` is
mounted without `BeforeMiddlewares`.

### Before vs. after middleware

`BeforeMiddlewares` run **before** the handler and own request control: they
can authenticate, populate `ctx.Auth`, or short-circuit (abort / set
`ctx.Response`) to replace the response entirely.

`AfterMiddlewares` run **after** the handler has served the request. Because
the handler streams its response (status, headers, body) straight to the
client, the response is already committed by the time an after-middleware runs —
so they are for **observation and side effects only** (audit logging, metrics,
cleanup), never for altering the response. Read the outcome with:

```go
AfterMiddlewares: []maniflex.MiddlewareFunc{
    func(ctx *maniflex.ServerContext, next func() error) error {
        status := ctx.Writer.(interface{ Status() int }).Status()
        log.Printf("file request completed: %d", status)
        return next()
    },
},
```

Setting `ctx.Response` from an after-middleware is ignored (and logged) rather
than corrupting the already-sent body. To alter, replace, or block the
response, use `BeforeMiddlewares`.

## Per-model attachment routes

For each `mfx:"file"` field on each model, the framework mounts a record-
scoped download path:

```
GET /:model/:id/:file_field
```

E.g. `GET /api/patients/123/discharge_summary` streams the file referenced
by `Patient.DischargeSummary` for record `123`.

Unlike `GET /files/{key...}`, this route runs through the **read pipeline**
for the parent record — the same `Auth`, soft-delete, and tenancy
middleware that protect `GET /api/patients/123` also protect the download.
Use this for any attachment whose access depends on the parent row.

Response codes:

| Status | Meaning |
|---|---|
| `200` | file streamed with `Content-Type`, `Content-Disposition`, `Content-Length` |
| `404 NOT_FOUND` | the record does not exist (or is soft-deleted) |
| `404 FILE_NOT_SET` | the record exists but the field is null/empty |
| `404 FILE_NOT_FOUND` | the field references a key that is missing from storage |
| `401 / 403` | whatever the Auth middleware decided |

The route is only mounted when `Config.FilesConfig.Storage` is configured; with
no storage backend, the route is absent and requests return `404` from the
router. (Unlike the standalone `/files` endpoints, per-model attachment routes
do not require `MountEndpoints`.)

Internally this is dispatched as a new operation, `maniflex.OpReadAttachment`.
Middleware filtered by `ForOperation(OpRead)` does **not** apply to
attachment requests; use `ForOperation(OpRead, OpReadAttachment)` to cover
both.

## Automatic cleanup

By default, a `file` field's stored bytes are removed when:

- the record is **hard-deleted**, or
- the field is **overwritten** by an update.

Setting `auto_delete:false` opts out, leaving the file in storage for
out-of-band lifecycle management. Soft-deleted rows never trigger cleanup —
the file is preserved until the row is hard-deleted.

## Bring-your-own storage

Implement `maniflex.FileStorage`:

```go
type FileStorage interface {
    Store(ctx context.Context, key string, r io.Reader, meta FileMeta) error
    Retrieve(ctx context.Context, key string) (io.ReadCloser, FileMeta, error)
    Delete(ctx context.Context, key string) error
    Exists(ctx context.Context, key string) (bool, error)
    Stat(ctx context.Context, key string) (FileMeta, error)
    PresignUpload(ctx context.Context, key string, opts PresignUploadOptions) (*PresignedUpload, error)
    URL(ctx context.Context, key string, ttl time.Duration) (string, error)
}
```

> **`Stat` and `PresignUpload` are new in v0.2.3** and break every third-party
> backend until it implements them.
>
> **`Stat`** returns an object's metadata without fetching its body. Return
> `maniflex.ErrFileNotFound` for a missing key. Its `Size` and `ContentType` are
> what a `file` field's `max_size` and `accept` are checked against when a record
> references a key, so they must describe what is really stored — a client that
> uploaded 5 GB to a 60 MB field is caught here or nowhere. If your backend has no
> cheap metadata call, `Retrieve` and measure; it is correct, merely slower.
>
> **`PresignUpload`** mints a direct-to-storage authorisation. If your backend
> cannot, **return `maniflex.ErrPresignUnsupported`** — the framework turns that
> into a clean `501`. Do *not* return an unsigned URL instead: that is not a
> degraded presigned upload, it is an unauthenticated write endpoint. Pin
> `opts.MaxSize` into the signature if you can (S3's POST policy does, via
> `content-length-range`; a presigned PUT cannot), since the framework's own check
> can only run once the bytes are already stored and paid for.

`Retrieve` returns `maniflex.ErrFileNotFound` when the key does not exist.
`Delete` *should* also return `maniflex.ErrFileNotFound` for missing keys so
the standalone `DELETE /files/*` handler can surface a 404 without an extra
`Exists` round-trip; backends that cannot detect the case atomically (e.g. S3
`DeleteObject` succeeds for missing keys) may return `nil` instead — both are
treated as "delete succeeded". `Store` is given a framework-generated key of
the form `uploads/<uuid>/<sanitised-filename>`; create any intermediate
directories or object prefixes as needed.

Storage backends are also expected to:

- honour `ctx` cancellation in `Store` — long uploads must abort when the
  request deadline elapses or the server is shutting down,
- reject keys ending in `.meta.json` in both `Store` and `Retrieve` if the
  backend uses sibling JSON files as a metadata layout (LocalStorage's case),
  so the framework's internal layout is never reachable through the file
  handler.

Filenames flowing through the framework-generated key are sanitised to the
charset `[A-Za-z0-9._-]` (other runes become `_`), leading dots are stripped,
and the result is truncated to 120 characters. CR / LF / NUL bytes never
survive into the storage key.

## File fields vs. static files

File fields handle *user-supplied* content. They are unrelated to
[Static Files](static-files.md), which serves a fixed directory of assets you
ship with the app.

| | File fields | Static files |
|---|---|---|
| Source | uploaded at runtime | committed to the repo |
| Storage | `FileStorage` backend | local disk |
| URL | `/files/<key>` | `/static/<path>` |
| Configured by | `Config.FilesConfig.Storage` | a `static/` directory |
