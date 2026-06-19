# 3. Modeling Domain Entities & Relations

With users in place, we model the catalogue. A book belongs to one author and
many genres, and accumulates many reviews — three relations, two flavours.

## The catalogue

Create the models. Each one lives in its own file under `models/`.

```go
// models/author.go
type Author struct {
    maniflex.BaseModel
    Name string `json:"name" mfx:"required,filterable,sortable"`
    Bio  string `json:"bio"`

    Books []Book `json:"books,omitempty"` // HasMany
}

// models/genre.go
type Genre struct {
    maniflex.BaseModel
    Label string `json:"label" mfx:"required,filterable,sortable,unique"`

    Books []Book `json:"books,omitempty" mfx:"through:BookGenre"`
}

// models/book.go
type Book struct {
    maniflex.BaseModel
    Title       string  `json:"title"        mfx:"required,filterable,sortable"`
    ISBN        string  `json:"isbn"         mfx:"required,filterable,unique"`
    Price       float64 `json:"price"        mfx:"required,min:0,filterable,sortable"`
    Stock       int64   `json:"stock"        mfx:"required,min:0,filterable"`
    PublishedAt string  `json:"published_at" mfx:"filterable,sortable"`

    AuthorID string `json:"author_id" mfx:"required,filterable"`        // BelongsTo Author

    Genres  []Genre  `json:"genres,omitempty"  mfx:"through:BookGenre"` // ManyToMany
    Reviews []Review `json:"reviews,omitempty"`                          // HasMany
}

// models/book_genre.go — the junction model for Book ↔ Genre.
type BookGenre struct {
    maniflex.BaseModel
    BookID  string `json:"book_id"  mfx:"required,filterable,immutable"`
    GenreID string `json:"genre_id" mfx:"required,filterable,immutable"`
}

// models/review.go
type Review struct {
    maniflex.BaseModel
    maniflex.WithDeletedAt
    BookID string `json:"book_id" mfx:"required,filterable,immutable"` // BelongsTo Book
    UserID string `json:"user_id" mfx:"required,filterable,immutable"` // BelongsTo User
    Rating int    `json:"rating"  mfx:"required,min:1,max:5,filterable,sortable"`
    Body   string `json:"body"    mfx:"required"`
}
```

Three relation styles in one place:

- **BelongsTo (convention)** — `Book.AuthorID` → `Author`, `Review.BookID` →
  `Book`, `Review.UserID` → `User`. No tags required; the framework reads the
  `ID` suffix.
- **HasMany** — `Author.Books`, `Book.Reviews`. A slice of the related struct,
  not a column on this table.
- **ManyToMany** — `Book.Genres` ↔ `Genre.Books` through the explicit
  `BookGenre` junction model. The `through:` tag names the junction; both
  sides declare it.

## Registering

All five models go to `MustRegister`:

```go
server.MustRegister(
    models.User{},
    models.Author{},
    models.Genre{},
    models.Book{},
    models.BookGenre{},
    models.Review{},
)
```

`AutoMigrate` creates the tables. The junction `book_genres` carries
`book_id` and `genre_id`; the framework wires the `Book ↔ Genre` relation
from the `through:` tag.

## Trying the relations

Create an author, a genre, and a book:

```bash
AUTH=$(curl -s -X POST localhost:8080/api/authors -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"Ursula K. Le Guin"}' | jq -r .data.id)

SCIFI=$(curl -s -X POST localhost:8080/api/genres -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"label":"Science Fiction"}' | jq -r .data.id)

BOOK=$(curl -s -X POST localhost:8080/api/books -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"The Dispossessed\",\"isbn\":\"9780061054884\",\"price\":12.99,\"stock\":10,\"author_id\":\"$AUTH\"}" \
  | jq -r .data.id)

# Tag it as sci-fi via the junction model.
curl -X POST localhost:8080/api/book_genres -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"book_id\":\"$BOOK\",\"genre_id\":\"$SCIFI\"}"
```

Now include the related rows in a read:

```bash
curl "localhost:8080/api/books/$BOOK?include=author,genres,reviews"
```

The response carries `author` (a single object), `genres` (an array), and
`reviews` (an empty array for now). Each include is a separate query against
the related table.

## Filtering through relations

Filters can traverse relations using dot notation. The related field must be
`filterable`:

```bash
# All books written by anyone whose name starts with "Ursula"
curl "localhost:8080/api/books?filter=author.name:ilike:Ursula%25"

# All books in the "Science Fiction" genre
curl "localhost:8080/api/books?filter=genres.label:eq:Science+Fiction&include=genres"
```

Filtering does not require an `include`; including merely returns the related
rows. You can join one and not the other freely.

## Cascading deletes

A deleted author should not orphan books. Update the `Book.AuthorID` tag to
declare a cascade:

```go
AuthorID string `json:"author_id" mfx:"required,filterable,relation:Author;onDelete:cascade"`
Author   Author `json:"author,omitempty"`
```

The companion `Author` field is now needed because the explicit `relation:`
tag must name a companion field of the target type. We also gain a slightly
better OpenAPI: the spec carries the relation explicitly.

`onDelete:setNull` and `onDelete:restrict` are the alternatives — see
[Relations](../defining-your-api/relations.md).

## What we built

| Concept | Where |
|---|---|
| BelongsTo (convention) | `Book.AuthorID`, `Review.BookID`, `Review.UserID` |
| HasMany | `Author.Books`, `Book.Reviews` |
| ManyToMany | `Book.Genres` ↔ `Genre.Books` via `BookGenre` |
| Explicit relation with cascade | `Book.AuthorID` after the cascade edit |
| Soft delete | `maniflex.WithDeletedAt` on `Review` |
| Filtering through relations | `?filter=author.name:ilike:Ursula%` |

## Next

In **[Part 4 — Validation & Business Rules](4-validation.md)** we tighten
the rules: ISBNs follow a specific format, a user may not review the same
book twice, and reviews on out-of-stock books are blocked.
