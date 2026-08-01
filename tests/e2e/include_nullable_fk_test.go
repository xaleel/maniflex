package e2e

// A nullable BelongsTo — the FK declared as *string, which is the only way to
// model an optional relation — never materialized under ?include=. The typed
// read path hands the include layer the struct field's own value (recordToMap
// does not unwrap pointers), and the FK was keyed with fmt.Sprint: for a
// *string that renders the ADDRESS ("0xc000112233"), so the batch fetch looked
// up ids that cannot exist and the relation was silently dropped — no error,
// just a missing key in the response.
//
// Every relation fixture in this suite used a non-pointer FK, which is why it
// went unnoticed.
//
//	go test ./tests/e2e/... -run TestIncludeNullableFK

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type nfkAuthor struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"filterable"`
}

// nfkBook mirrors the shape an optional relation has to take: a *string FK
// (NULLable column, onDelete:set_null-able) plus its companion field.
type nfkBook struct {
	maniflex.BaseModel
	Title       string     `json:"title" db:"title"`
	NfkAuthorID *string    `json:"nfk_author_id,omitempty" db:"nfk_author_id" mfx:"filterable,relation"`
	NfkAuthor   *nfkAuthor `json:"nfk_author,omitempty"`
}

func nfkServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{nfkAuthor{}, nfkBook{}},
	})
}

func TestIncludeNullableFK_GetAndList(t *testing.T) {
	srv := nfkServer(t)

	aid := srv.MustID(srv.POST("/nfk_authors", map[string]any{"name": "Ursula"}))
	bid := srv.MustID(srv.POST("/nfk_books",
		map[string]any{"title": "The Dispossessed", "nfk_author_id": aid}))

	body := string(srv.GET("/nfk_books/" + bid + "?include=nfk_author").Body)
	if !strings.Contains(body, "Ursula") {
		t.Errorf("get by id: ?include= on a *string FK did not populate the "+
			"related row: %s", body)
	}

	body = string(srv.GET("/nfk_books?include=nfk_author").Body)
	if !strings.Contains(body, "Ursula") {
		t.Errorf("list: ?include= on a *string FK did not populate the "+
			"related row: %s", body)
	}
}

// A row whose optional FK is genuinely NULL must come back without the relation
// key and without disturbing the rows that do carry one.
//
// Asserted per row rather than by substring over the whole body: a body-wide
// check for "Ursula" passes just as happily when the author is attached to the
// WRONG book, which is the failure a keying bug is most likely to produce.
// Decoding to json.RawMessage also separates "key absent" (correct) from a
// present-but-empty "nfk_author": null / {}.
func TestIncludeNullableFK_NullFKIsSkipped(t *testing.T) {
	srv := nfkServer(t)

	aid := srv.MustID(srv.POST("/nfk_authors", map[string]any{"name": "Ursula"}))
	srv.POST("/nfk_books", map[string]any{"title": "The Dispossessed", "nfk_author_id": aid})
	srv.POST("/nfk_books", map[string]any{"title": "Anonymous"})

	res := srv.GET("/nfk_books?include=nfk_author")
	res.AssertStatus(200)

	var out struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatalf("decode list: %v: %s", err, res.Body)
	}
	if len(out.Data) != 2 {
		t.Fatalf("want 2 books, got %d: %s", len(out.Data), res.Body)
	}

	for _, row := range out.Data {
		var title string
		if err := json.Unmarshal(row["title"], &title); err != nil {
			t.Fatalf("decode title: %v: %s", err, res.Body)
		}
		rel, present := row["nfk_author"]

		switch title {
		case "The Dispossessed":
			if !present {
				t.Errorf("the row that has an author must still be included: %s", res.Body)
				continue
			}
			var author struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(rel, &author); err != nil {
				t.Fatalf("decode nfk_author: %v: %s", err, rel)
			}
			if author.Name != "Ursula" {
				t.Errorf("nfk_author.name = %q, want %q", author.Name, "Ursula")
			}
		case "Anonymous":
			if present {
				t.Errorf("a NULL FK must leave the relation key off entirely, got %s: %s",
					rel, res.Body)
			}
		default:
			t.Errorf("unexpected book %q in the response", title)
		}
	}
}

// The typed companion field (nfkBook.NfkAuthor) must be populated too, not just
// the JSON carrier — the typed read helpers are the other consumer.
func TestIncludeNullableFK_PopulatesTypedCompanion(t *testing.T) {
	var got string
	var sawRow bool

	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{nfkAuthor{}, nfkBook{}},
		Middleware: func(s *maniflex.Server) {
			s.Action(maniflex.ActionConfig{
				Method: "GET", Path: "/typed-nfk-books",
				Handler: func(ctx *maniflex.ServerContext) error {
					books, err := maniflex.List[nfkBook](ctx, &maniflex.QueryParams{
						Page: 1, Limit: 10, Includes: []string{"nfk_author"},
					})
					if err != nil {
						return err
					}
					for _, b := range books {
						sawRow = true
						if b.NfkAuthor != nil {
							got = b.NfkAuthor.Name
						}
					}
					ctx.Response = &maniflex.APIResponse{StatusCode: 200}
					return nil
				},
			})
		},
	})

	aid := srv.MustID(srv.POST("/nfk_authors", map[string]any{"name": "Ursula"}))
	srv.POST("/nfk_books", map[string]any{"title": "The Dispossessed", "nfk_author_id": aid})

	srv.GET("/typed-nfk-books").AssertStatus(200)
	if !sawRow {
		t.Fatal("typed List returned no rows")
	}
	if got != "Ursula" {
		t.Errorf("book.NfkAuthor.Name = %q, want %q (typed companion not populated)", got, "Ursula")
	}
}
