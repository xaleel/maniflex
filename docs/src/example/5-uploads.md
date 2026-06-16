# 5. File Uploads

A book needs a cover image. In this part we add a file field to the `Book`
model, configure local storage for development, and learn how the same code
handles a swap to S3 in production.

## Adding the file field

Edit `models/book.go`:

```go
type Book struct {
    maniflex.BaseModel
    Title       string  `json:"title"        mfx:"required,filterable,sortable"`
    ISBN        string  `json:"isbn"         mfx:"required,filterable,unique"`
    Price       float64 `json:"price"        mfx:"required,min:0,filterable,sortable"`
    Stock       int64   `json:"stock"        mfx:"required,min:0,filterable"`
    PublishedAt string  `json:"published_at" mfx:"filterable,sortable"`

    AuthorID string `json:"author_id" mfx:"required,filterable,relation:Author;onDelete:cascade"`
    Author   Author `json:"author,omitempty"`

    Cover string `json:"cover" mfx:"file,max_size:2MB,accept:image/png|image/jpeg"`

    Genres  []Genre  `json:"genres,omitempty"  mfx:"through:BookGenre"`
    Reviews []Review `json:"reviews,omitempty"`
}
```

The `cover` field is a string in Go and a string in the database, but the
`mfx:"file"` tag opts the model into multipart uploads. The column stores the
*storage key* — a path under whichever backend you have configured.

`max_size` and `accept` are enforced in the framework, before the upload
reaches storage. A 5 MB JPEG or a `application/pdf` is rejected with
`400 BAD_REQUEST` and never written.

## Configuring storage

For development we use local disk. The `maniflex/storage` package ships a ready
implementation:

```go
import "github.com/xaleel/maniflex/storage"

fs, err := storage.NewLocalStorage("./uploads")
if err != nil {
    log.Fatal(err)
}

server := maniflex.New(maniflex.Config{
    Port:        8080,
    PathPrefix:  "/api",
    AutoMigrate: true,
    FileStorage: fs,
})
```

`./uploads` is created if it doesn't exist. Every uploaded file lands under
`uploads/<uuid>/<sanitised-filename>` so collisions are impossible.

## Uploading a cover

There are two ways to attach a cover, both supported out of the box.

### 1. Multipart upload alongside create

The client sends `multipart/form-data` with one part per field:

```bash
curl -X POST localhost:8080/api/books \
  -H "Authorization: Bearer $TOKEN" \
  -F 'title=The Dispossessed' \
  -F 'isbn=9780061054884' \
  -F 'price=12.99' \
  -F 'stock=10' \
  -F "author_id=$AUTH" \
  -F 'cover=@./covers/dispossessed.jpg;type=image/jpeg'
```

The framework parses the multipart envelope, streams `cover` into the
storage backend, writes the resulting key into the column, and persists the
row. The response is the usual JSON envelope:

```json
{
  "data": {
    "id": "...",
    "title": "The Dispossessed",
    "cover": "uploads/3f2b.../dispossessed.jpg",
    ...
  }
}
```

### 2. Two-step upload + reference

For large files or out-of-band uploads, hit the standalone
[`/files`](../files.md#standalone-file-endpoints) endpoint first:

```bash
KEY=$(curl -s -X POST localhost:8080/files \
  -H "Authorization: Bearer $TOKEN" \
  -F 'file=@./covers/dispossessed.jpg;type=image/jpeg' \
  | jq -r .data.key)

curl -X POST localhost:8080/api/books \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"The Dispossessed\",\"isbn\":\"9780061054884\",\"price\":12.99,\"stock\":10,\"author_id\":\"$AUTH\",\"cover\":\"$KEY\"}"
```

The `file` field accepts a plain string in JSON — the storage key returned
by `/files`. The framework recognises that the value is already a key (not a
new upload) and stores it as-is.

## Downloading a cover

Storage keys are served at `/files/{key...}`:

```bash
curl 'localhost:8080/files/uploads/3f2b.../dispossessed.jpg' --output cover.jpg
```

The handler sets `Content-Type`, `Content-Disposition: inline`, and
`Content-Length` from the metadata stored alongside the file.

For a permission layer in front of downloads — say, only registered users
can fetch covers — add Auth middleware to the file route just as you would
for any model route.

## Automatic cleanup

The framework tracks the row that owns each key. A file is deleted from
storage when:

- the owning row is **hard-deleted**, or
- the field is **overwritten** by a `PATCH` that supplies a new file or key.

`Book` does not embed `WithDeletedAt`, so a delete is a hard-delete and the
cover goes away too. If you want covers to outlive book deletions (for an
audit trail), tag the field with `auto_delete:false`:

```go
Cover string `json:"cover" mfx:"file,max_size:2MB,accept:image/*,auto_delete:false"`
```

## Swapping in S3

`FileStorage` is a four-method interface — `Store`, `Retrieve`, `Delete`,
`Exists`. A drop-in S3 implementation looks like:

```go
type S3Storage struct{ client *s3.Client; bucket string }

func (s *S3Storage) Store(ctx context.Context, key string, r io.Reader, meta maniflex.FileMeta) error {
    _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
        Bucket:      &s.bucket,
        Key:         &key,
        Body:        r,
        ContentType: &meta.ContentType,
    })
    return err
}

// Retrieve, Delete, Exists similarly.
```

Swap `storage.NewLocalStorage(...)` for the new type in `main.go` and nothing
else changes. The same model code, the same endpoints, the same multipart
parser. The model never knows.

## What we built

| Capability | How |
|---|---|
| File field on Book | `mfx:"file,max_size:...,accept:..."` |
| Local storage backend | `storage.NewLocalStorage("./uploads")` |
| Multipart upload | The framework auto-detects `multipart/form-data` on create/update |
| Pre-uploaded key reference | Plain string in the JSON body |
| Standalone upload | `POST /files`, returns a key |
| Backend-agnostic | `maniflex.FileStorage` interface — swap to S3 with no model change |

## Next

In **[Part 6 — Filtering, Sorting & Pagination](6-querying.md)** we build a
catalogue browser: lookup books by title, sort by price or publication date,
paginate the results, and combine includes with filters.
