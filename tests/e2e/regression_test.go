package e2e

// regression_test.go covers the 12 gaps identified after auditing the initial
// e2e suite. Each test is annotated with the bug category it targets.
//
// Critical (7):
//   C1 - mfx:"hidden" field never appears in any response
//   C2 - mfx:"min" / mfx:"max" numeric validation is enforced end-to-end
//   C3 - mfx:"unique" constraint rejects duplicate values at the DB level
//   C4 - Multi-sort (two sort fields) produces correct ordering
//   C5 - Nested filter on a HasMany relation key is rejected with 400
//   C6 - response.Cache sets Cache-Control + ETag and honours If-None-Match
//   C7 - response.RedactField conditionally removes a field from responses
//
// High (5):
//   H1 - ?include= response keeps both the raw FK field and the embedded object
//   H2 - ?include= list correctly pairs each nested object to its own parent row
//   H3 - Concurrent writes to the same record all return 200 (no data corruption)
//   H4 - PATCH with empty body {} returns 200 with unchanged data
//   H5 - Multiple Replace registrations: last one wins, first is never called

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/middleware/validate"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Local test models ─────────────────────────────────────────────────────────
// These supplement the shared fixtures with fields needed to cover the missing
// tag surface (hidden, min, max). They are only registered in tests that need them.

// SecretDoc exercises mfx:"hidden" on a field that must never appear in responses.
type SecretDoc struct {
	maniflex.BaseModel
	Title  string `json:"title"  db:"title"  mfx:"required,filterable,sortable"`
	Secret string `json:"secret" db:"secret" mfx:"hidden"`
}

// RatedItem exercises mfx:"min" and mfx:"max" on a numeric field.
type RatedItem struct {
	maniflex.BaseModel
	Name  string `json:"name"  db:"name"  mfx:"required"`
	Score int    `json:"score" db:"score" mfx:"required,filterable,min:1,max:10"`
}

// ── C1: mfx:"hidden" field never appears in any response ─────────────────────

func TestCritical_HiddenField(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{SecretDoc{}}})

	t.Run("absent_from_create_response", func(t *testing.T) {
		t.Parallel()
		resp := srv.POST("/secret_docs", map[string]any{"title": "Doc", "secret": "topsecret"})
		resp.AssertStatus(http.StatusCreated)
		if _, ok := resp.Data()["secret"]; ok {
			t.Error("hidden field 'secret' must not appear in create response")
		}
	})

	t.Run("absent_from_read_response", func(t *testing.T) {
		t.Parallel()
		id := srv.MustID(srv.POST("/secret_docs", map[string]any{"title": "D2", "secret": "classified"}))
		if _, ok := srv.GET("/secret_docs/" + id).Data()["secret"]; ok {
			t.Error("hidden field 'secret' must not appear in read response")
		}
	})

	t.Run("absent_from_all_list_items", func(t *testing.T) {
		t.Parallel()
		srv2 := testutil.NewServer(t, testutil.Options{Models: []any{SecretDoc{}}})
		srv2.MustID(srv2.POST("/secret_docs", map[string]any{"title": "A", "secret": "s1"}))
		srv2.MustID(srv2.POST("/secret_docs", map[string]any{"title": "B", "secret": "s2"}))
		for _, item := range srv2.GET("/secret_docs").DataList() {
			if _, ok := item.(map[string]any)["secret"]; ok {
				t.Errorf("hidden 'secret' must not appear in list items, got: %v", item)
			}
		}
	})

	t.Run("absent_from_update_response", func(t *testing.T) {
		t.Parallel()
		id := srv.MustID(srv.POST("/secret_docs", map[string]any{"title": "D3", "secret": "x"}))
		if _, ok := srv.PATCH("/secret_docs/"+id, map[string]any{"title": "Updated"}).Data()["secret"]; ok {
			t.Error("hidden field 'secret' must not appear in update response")
		}
	})

	t.Run("record_is_stored_and_readable_by_other_fields", func(t *testing.T) {
		// Hidden means absent from HTTP responses, not absent from the DB.
		// The record must still be retrievable; only the secret column is suppressed.
		t.Parallel()
		id := srv.MustID(srv.POST("/secret_docs", map[string]any{"title": "Stored", "secret": "persisted"}))
		data := srv.GET("/secret_docs/" + id).Data()
		testutil.AssertEqual(t, "title visible", testutil.Field(t, data, "title"), "Stored")
	})
}

// ── C2: mfx:"min" / mfx:"max" numeric validation ─────────────────────────────

func TestCritical_MinMaxValidation(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{RatedItem{}}})

	t.Run("below_min_returns_422", func(t *testing.T) {
		t.Parallel()
		srv.POST("/rated_items", map[string]any{"name": "Low", "score": 0}).
			AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("above_max_returns_422", func(t *testing.T) {
		t.Parallel()
		srv.POST("/rated_items", map[string]any{"name": "High", "score": 11}).
			AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("at_min_boundary_passes", func(t *testing.T) {
		t.Parallel()
		srv.POST("/rated_items", map[string]any{"name": "MinOK", "score": 1}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("at_max_boundary_passes", func(t *testing.T) {
		t.Parallel()
		srv.POST("/rated_items", map[string]any{"name": "MaxOK", "score": 10}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("within_range_passes", func(t *testing.T) {
		t.Parallel()
		srv.POST("/rated_items", map[string]any{"name": "Mid", "score": 5}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("violation_on_update_returns_422", func(t *testing.T) {
		t.Parallel()
		id := srv.MustID(srv.POST("/rated_items", map[string]any{"name": "V", "score": 5}))
		srv.PATCH("/rated_items/"+id, map[string]any{"score": 99}).
			AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("absent_field_on_patch_skips_validation", func(t *testing.T) {
		// Omitting the constrained field on PATCH must not trigger min/max.
		t.Parallel()
		id := srv.MustID(srv.POST("/rated_items", map[string]any{"name": "V2", "score": 5}))
		srv.PATCH("/rated_items/"+id, map[string]any{"name": "Renamed"}).
			AssertStatus(http.StatusOK)
	})

	t.Run("error_code_is_validation_error", func(t *testing.T) {
		t.Parallel()
		resp := srv.POST("/rated_items", map[string]any{"name": "Bad", "score": 0})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "VALIDATION_ERROR")
	})
}

// ── C3: mfx:"unique" constraint rejects duplicate values ─────────────────────

func TestCritical_UniqueConstraint(t *testing.T) {
	t.Parallel()

	t.Run("db_level_unique_rejects_duplicate", func(t *testing.T) {
		// Tests the UNIQUE column in the migrated schema — not middleware.
		// A missing UNIQUE constraint in AutoMigrate would let both creates succeed.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.POST("/users", map[string]any{
			"name": "A", "email": "dup@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)

		resp := srv.POST("/users", map[string]any{
			"name": "B", "email": "dup@x.com", "password": "s",
		})
		if resp.Status == http.StatusCreated {
			t.Error("duplicate unique email must not produce 201 Created")
		}
	})

	t.Run("different_emails_both_succeed", func(t *testing.T) {
		// Control case: two different emails must both work.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.POST("/users", map[string]any{
			"name": "A", "email": "uniq1@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)
		srv.POST("/users", map[string]any{
			"name": "B", "email": "uniq2@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("validate_unique_field_middleware_with_sqlite_driver", func(t *testing.T) {
		// validate.UniqueField now takes an explicit driver argument and emits
		// dialect-correct placeholders (? for SQLite, $N for Postgres). This
		// test exercises the SQLite path end-to-end against a real adapter.
		t.Parallel()

		rawDB, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open raw db: %v", err)
		}
		rawDB.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;")
		t.Cleanup(func() { rawDB.Close() })

		server := maniflex.New(maniflex.Config{PathPrefix: "/api"})
		server.MustRegister(testutil.User{})

		adapter := sqlcore.New(rawDB, rawDB, maniflex.SQLite, server.Registry())
		adapter.SetErrorNormalizer(sqlite.NormalizeError)
		server.SetDB(adapter)
		if err := adapter.AutoMigrate(context.Background(), server.Registry()); err != nil {
			t.Fatalf("migrate: %v", err)
		}

		server.Pipeline.Validate.Register(
			validate.UniqueField(rawDB, maniflex.SQLite, "email"),
			maniflex.ForModel("User"),
		)

		ts := httptest.NewServer(server.Handler())
		t.Cleanup(ts.Close)

		// First create: must succeed
		if s := postJSON(t, ts.URL+"/api/users", map[string]any{
			"name": "A", "email": "vmw@x.com", "password": "s",
		}); s != http.StatusCreated {
			t.Fatalf("first create: got %d, want 201", s)
		}

		// Duplicate: validate.UniqueField must intercept with 422
		s := postJSON(t, ts.URL+"/api/users", map[string]any{
			"name": "B", "email": "vmw@x.com", "password": "s",
		})
		if s == http.StatusCreated {
			t.Error("duplicate email: validate.UniqueField should have rejected with 422")
		}
		if s != http.StatusUnprocessableEntity {
			t.Errorf("expected 422, got %d", s)
		}
	})
}

// ── C4: Multi-sort (two sort fields) produces correct ordering ────────────────

func TestCritical_MultiSort(t *testing.T) {
	t.Parallel()

	t.Run("two_fields_both_asc_correct_order", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// Two admins and two editors; name is the tiebreaker within each role.
		srv.MustID(srv.CreateUser("Charlie", "c@ms.com", "editor"))
		srv.MustID(srv.CreateUser("Alice", "a@ms.com", "admin"))
		srv.MustID(srv.CreateUser("Bob", "b@ms.com", "admin"))
		srv.MustID(srv.CreateUser("Dave", "d@ms.com", "editor"))

		items := srv.GET("/users?sort=role:asc,name:asc").DataList()
		if len(items) != 4 {
			t.Fatalf("expected 4 items, got %d", len(items))
		}
		want := []struct{ name, role string }{
			{"Alice", "admin"},
			{"Bob", "admin"},
			{"Charlie", "editor"},
			{"Dave", "editor"},
		}
		for i, w := range want {
			m := items[i].(map[string]any)
			gotName := testutil.Field(t, m, "name")
			gotRole := testutil.Field(t, m, "role")
			if gotName != w.name || gotRole != w.role {
				t.Errorf("[%d] got %q/%q, want %q/%q", i, gotName, gotRole, w.name, w.role)
			}
		}
	})

	t.Run("second_field_desc_breaks_ties_in_reverse", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.CreateUser("Zara", "z@ms.com", "admin"))
		srv.MustID(srv.CreateUser("Anna", "an@ms.com", "admin"))
		srv.MustID(srv.CreateUser("Xena", "x@ms.com", "viewer"))

		// role:asc, name:desc → within admin: Zara then Anna
		items := srv.GET("/users?sort=role:asc,name:desc").DataList()
		if len(items) != 3 {
			t.Fatalf("expected 3, got %d", len(items))
		}
		testutil.AssertEqual(t, "1st admin desc", testutil.Field(t, items[0].(map[string]any), "name"), "Zara")
		testutil.AssertEqual(t, "2nd admin desc", testutil.Field(t, items[1].(map[string]any), "name"), "Anna")
	})

	t.Run("first_sort_field_alone_cannot_determine_second_tie", func(t *testing.T) {
		// Sanity: if we sort by only the primary field, ties are not deterministically broken.
		// With two fields, ordering must be stable and correct.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.CreateUser("Zed", "zed@ms.com", "admin"))
		srv.MustID(srv.CreateUser("Amy", "amy@ms.com", "admin"))

		// With multi-sort role:asc,name:asc both are admin — name must break the tie
		items := srv.GET("/users?sort=role:asc,name:asc").DataList()
		if len(items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(items))
		}
		testutil.AssertEqual(t, "Amy before Zed",
			testutil.Field(t, items[0].(map[string]any), "name"), "Amy")
	})
}

// ── C5: Nested filter on HasMany relation key rejected with 400 ───────────────

func TestCritical_NestedFilterHasManyRejected(t *testing.T) {
	t.Parallel()

	t.Run("has_many_relation_key_in_nested_filter_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// "comments" is HasMany on Post — nested filter through it is unsupported
		srv.GET("/posts?filter=comments.approved:eq:true").AssertStatus(http.StatusBadRequest)
	})

	t.Run("has_many_on_user_model_rejected", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// "posts" is HasMany on User
		srv.GET("/users?filter=posts.status:eq:published").AssertStatus(http.StatusBadRequest)
	})

	t.Run("belongs_to_still_works_as_control", func(t *testing.T) {
		// BelongsTo nested filter must still return 200, proving we're testing
		// HasMany rejection specifically and not breaking all nested filters.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/posts?filter=user.role:eq:admin").AssertStatus(http.StatusOK)
	})

	t.Run("unknown_relation_key_rejected_with_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/posts?filter=category.name:eq:tech").AssertStatus(http.StatusBadRequest)
	})
}

// ── C6: response.Cache — Cache-Control, ETag, 304 Not Modified ───────────────

func TestCritical_CacheMiddleware(t *testing.T) {
	t.Parallel()

	newSrv := func(t *testing.T) *testutil.Server {
		t.Helper()
		return testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Cache(300),
					maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
	}

	t.Run("cache_control_set_on_read", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "cc@x.com", "viewer"))
		resp := srv.GET("/users/" + id)
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "Cache-Control", resp.Header.Get("Cache-Control"), "public, max-age=300")
	})

	t.Run("cache_control_set_on_list", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		srv.MustID(srv.CreateUser("U", "ccl@x.com", "viewer"))
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		if resp.Header.Get("Cache-Control") == "" {
			t.Error("Cache-Control must be set on list responses")
		}
	})

	t.Run("etag_present_on_read", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "etag@x.com", "viewer"))
		etag := srv.GET("/users/" + id).Header.Get("ETag")
		testutil.AssertNotEmpty(t, "ETag header", etag)
	})

	t.Run("etag_is_deterministic", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "det@x.com", "viewer"))
		e1 := srv.GET("/users/" + id).Header.Get("ETag")
		e2 := srv.GET("/users/" + id).Header.Get("ETag")
		testutil.AssertNotEmpty(t, "etag", e1)
		testutil.AssertEqual(t, "etags match", e1, e2)
	})

	t.Run("if_none_match_returns_304_empty_body", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "304u@x.com", "viewer"))
		etag := srv.GET("/users/" + id).Header.Get("ETag")
		testutil.AssertNotEmpty(t, "etag for 304 test", etag)

		resp := srv.Do("GET", srv.APIPath("/users/"+id), nil,
			map[string]string{"If-None-Match": etag})
		resp.AssertStatus(http.StatusNotModified)
		if len(resp.Body) > 0 {
			t.Errorf("304 must have empty body, got: %s", resp.Body)
		}
	})

	t.Run("etag_changes_after_update", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "etagupd@x.com", "viewer"))
		e1 := srv.GET("/users/" + id).Header.Get("ETag")
		srv.PATCH("/users/"+id, map[string]any{"name": "Updated"})
		e2 := srv.GET("/users/" + id).Header.Get("ETag")
		if e1 == e2 {
			t.Error("ETag must change after the record is updated")
		}
	})

	t.Run("cache_not_set_on_create", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		resp := srv.POST("/users", map[string]any{
			"name": "U", "email": "nocache@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
		if cc := resp.Header.Get("Cache-Control"); cc != "" {
			t.Errorf("Cache-Control must not be set on create response, got: %q", cc)
		}
	})

	t.Run("wrong_etag_returns_200_not_304", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("U", "wrongetag@x.com", "viewer"))
		resp := srv.Do("GET", srv.APIPath("/users/"+id), nil,
			map[string]string{"If-None-Match": `"wrong-etag-value"`})
		resp.AssertStatus(http.StatusOK)
	})
}

// ── C7: response.RedactField conditionally removes a field ───────────────────

func TestCritical_RedactField(t *testing.T) {
	t.Parallel()

	// X-Admin: true → show role; anything else → redact role
	newSrv := func(t *testing.T) *testutil.Server {
		t.Helper()
		return testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.RedactField("role", func(ctx *maniflex.ServerContext) bool {
						return ctx.Request.Header.Get("X-Admin") != "true"
					}),
					maniflex.ForModel("User"),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
	}

	t.Run("field_absent_when_condition_true", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("Alice", "rd1@x.com", "admin"))
		data := srv.GET("/users/" + id).Data()
		if _, ok := data["role"]; ok {
			t.Error("role must be redacted for non-admin callers")
		}
	})

	t.Run("field_present_when_condition_false", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("Alice", "rd2@x.com", "admin"))
		data := srv.GET("/users/"+id, map[string]string{"X-Admin": "true"}).Data()
		if _, ok := data["role"]; !ok {
			t.Error("role must be visible for admin callers")
		}
	})

	t.Run("redact_applies_to_all_list_items", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		srv.MustID(srv.CreateUser("A", "rdl1@x.com", "admin"))
		srv.MustID(srv.CreateUser("B", "rdl2@x.com", "editor"))
		for _, item := range srv.GET("/users").DataList() {
			if _, ok := item.(map[string]any)["role"]; ok {
				t.Errorf("role must be redacted in list, got: %v", item)
			}
		}
	})

	t.Run("redact_does_not_remove_other_fields", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		id := srv.MustID(srv.CreateUser("Alice", "rd3@x.com", "admin"))
		data := srv.GET("/users/" + id).Data()
		testutil.AssertNotEmpty(t, "name still present", testutil.Field(t, data, "name"))
		testutil.AssertNotEmpty(t, "id still present", testutil.Field(t, data, "id"))
	})

	t.Run("redact_scoped_to_model_does_not_affect_others", func(t *testing.T) {
		t.Parallel()
		srv := newSrv(t)
		u := srv.MustID(srv.CreateUser("U", "rd4@x.com", "viewer"))
		pid := srv.MustID(srv.CreatePost("Post", "draft", u))
		// Post has "status" — must not be redacted by User's RedactField
		data := srv.GET("/posts/" + pid).Data()
		testutil.AssertNotEmpty(t, "status present on Post", testutil.Field(t, data, "status"))
	})
}

// ── H1: ?include= keeps both the raw FK field AND the embedded object ─────────

func TestHigh_IncludeRetainsFKField(t *testing.T) {
	t.Parallel()

	t.Run("user_id_and_user_object_both_present_on_single_read", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		uid := srv.MustID(srv.CreateUser("Alice", "fk1@x.com", "admin"))
		pid := srv.MustID(srv.CreatePost("Post", "published", uid))

		data := srv.GET("/posts/" + pid + "?include=user").Data()

		// Raw FK field must survive
		gotFK := testutil.Field(t, data, "user_id")
		testutil.AssertEqual(t, "user_id present and correct", gotFK, uid)

		// Embedded object must also be present
		user, ok := data["user"].(map[string]any)
		if !ok {
			t.Fatalf("expected embedded 'user' object, got %T", data["user"])
		}
		testutil.AssertEqual(t, "embedded user.id == user_id",
			testutil.Field(t, user, "id"), uid)
	})

	t.Run("user_id_and_user_object_both_present_on_list", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		uid := srv.MustID(srv.CreateUser("Bob", "fk2@x.com", "editor"))
		srv.MustID(srv.CreatePost("P1", "draft", uid))
		srv.MustID(srv.CreatePost("P2", "draft", uid))

		for _, item := range srv.GET("/posts?include=user").DataList() {
			m := item.(map[string]any)
			rawFK := testutil.Field(t, m, "user_id")
			testutil.AssertNotEmpty(t, "user_id in list item", rawFK)

			user, ok := m["user"].(map[string]any)
			if !ok {
				t.Errorf("list item missing embedded user: %v", m)
				continue
			}
			// The embedded id must match the FK
			testutil.AssertEqual(t, "embedded user.id == user_id",
				testutil.Field(t, user, "id"), rawFK)
		}
	})
}

// ── H2: ?include= list correctly pairs each object to its own parent ──────────

func TestHigh_IncludePairing(t *testing.T) {
	t.Parallel()

	t.Run("each_post_embeds_its_own_author_not_a_shared_one", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u1 := srv.MustID(srv.CreateUser("Alice", "pr1@x.com", "admin"))
		u2 := srv.MustID(srv.CreateUser("Bob", "pr2@x.com", "editor"))
		srv.MustID(srv.CreatePost("AlicePost", "published", u1))
		srv.MustID(srv.CreatePost("BobPost", "draft", u2))

		for _, item := range srv.GET("/posts?include=user").DataList() {
			m := item.(map[string]any)
			title := testutil.Field(t, m, "title")
			userID := testutil.Field(t, m, "user_id")
			user, ok := m["user"].(map[string]any)
			if !ok {
				t.Errorf("post %q missing embedded user", title)
				continue
			}
			embeddedID := testutil.Field(t, user, "id")
			if embeddedID != userID {
				t.Errorf("post %q: embedded user.id=%q ≠ user_id=%q", title, embeddedID, userID)
			}
			embeddedName := testutil.Field(t, user, "name")
			if title == "AlicePost" && embeddedName != "Alice" {
				t.Errorf("AlicePost has wrong author: %q", embeddedName)
			}
			if title == "BobPost" && embeddedName != "Bob" {
				t.Errorf("BobPost has wrong author: %q", embeddedName)
			}
		}
	})

	t.Run("has_many_children_belong_to_their_own_parent", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "pr3@x.com", "viewer"))
		p1 := srv.MustID(srv.CreatePost("P1", "published", u))
		p2 := srv.MustID(srv.CreatePost("P2", "draft", u))
		srv.MustID(srv.CreateComment("C1", p1, u))
		srv.MustID(srv.CreateComment("C2", p1, u))
		srv.MustID(srv.CreateComment("C3", p2, u))

		for _, item := range srv.GET("/posts?include=comments").DataList() {
			m := item.(map[string]any)
			postID := testutil.Field(t, m, "id")
			comments, ok := m["comments"].([]any)
			if !ok {
				t.Errorf("post %q missing comments array", postID)
				continue
			}
			for _, c := range comments {
				cm := c.(map[string]any)
				cmPostID := testutil.Field(t, cm, "post_id")
				if cmPostID != postID {
					t.Errorf("comment.post_id=%q does not match parent id=%q", cmPostID, postID)
				}
			}
		}
	})

	t.Run("correct_child_counts_per_parent", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "pr4@x.com", "viewer"))
		p1 := srv.MustID(srv.CreatePost("P1", "published", u))
		p2 := srv.MustID(srv.CreatePost("P2", "draft", u))
		srv.MustID(srv.CreateComment("C1", p1, u))
		srv.MustID(srv.CreateComment("C2", p1, u))
		srv.MustID(srv.CreateComment("C3", p2, u))

		for _, item := range srv.GET("/posts?include=comments").DataList() {
			m := item.(map[string]any)
			title := testutil.Field(t, m, "title")
			comments := m["comments"].([]any)
			if title == "P1" && len(comments) != 2 {
				t.Errorf("P1 should have 2 comments, got %d", len(comments))
			}
			if title == "P2" && len(comments) != 1 {
				t.Errorf("P2 should have 1 comment, got %d", len(comments))
			}
		}
	})
}

// ── H3: Concurrent writes all return 200 (no corruption or locking errors) ───

func TestHigh_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	t.Run("five_concurrent_patches_to_same_record_all_succeed", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Concurrent", "con@x.com", "viewer"))

		const n = 5
		statuses := make([]int, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statuses[i] = srv.PATCH("/users/"+id,
					map[string]any{"name": fmt.Sprintf("Name%d", i)}).Status
			}(i)
		}
		wg.Wait()

		for i, s := range statuses {
			if s != http.StatusOK {
				t.Errorf("goroutine %d: got %d, want 200", i, s)
			}
		}
		// Record still readable
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
	})

	t.Run("ten_concurrent_creates_produce_unique_ids", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})

		const n = 10
		var wg sync.WaitGroup
		var mu sync.Mutex
		ids := make([]string, 0, n)
		statuses := make([]int, n)

		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				resp := srv.CreateUser(
					fmt.Sprintf("User%d", i),
					fmt.Sprintf("con_12_%d@x.com", i),
					"viewer",
				)
				statuses[i] = resp.Status
				if resp.Status == http.StatusCreated {
					mu.Lock()
					ids = append(ids, resp.ID())
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()

		for i, s := range statuses {
			if s != http.StatusCreated {
				t.Errorf("goroutine %d: got %d, want 201", i, s)
			}
		}
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if seen[id] {
				t.Errorf("duplicate ID produced: %q", id)
			}
			seen[id] = true
		}
	})
}

// ── H4: PATCH with empty body {} returns 200 with unchanged data ──────────────

func TestHigh_PatchEmptyBody(t *testing.T) {
	t.Parallel()

	t.Run("empty_object_returns_200_unchanged_record", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "empty@x.com", "admin"))

		resp := srv.PATCH("/users/"+id, map[string]any{})
		resp.AssertStatus(http.StatusOK)
		data := resp.Data()
		testutil.AssertEqual(t, "name unchanged", testutil.Field(t, data, "name"), "Alice")
		testutil.AssertEqual(t, "email unchanged", testutil.Field(t, data, "email"), "empty@x.com")
		testutil.AssertEqual(t, "role unchanged", testutil.Field(t, data, "role"), "admin")
	})

	t.Run("empty_object_id_unchanged", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "empid@x.com", "viewer"))
		resp := srv.PATCH("/users/"+id, map[string]any{})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "id unchanged", testutil.Field(t, resp.Data(), "id"), id)
	})

	t.Run("patch_only_readonly_fields_stripped_to_empty_does_not_500", func(t *testing.T) {
		// After the validate step strips readonly fields, the body may be empty.
		// This must not cause a 500 — the adapter handles empty data gracefully.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("U", "rdonly@x.com", "viewer"))
		pid := srv.MustID(srv.CreatePost("P", "draft", u))
		// 'views' is readonly — stripped to nothing
		resp := srv.PATCH("/posts/"+pid, map[string]any{"views": 999})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "views unchanged", testutil.FloatField(t, resp.Data(), "views"), float64(0))
	})

	t.Run("empty_patch_does_not_corrupt_existing_data", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Original", "corrupt@x.com", "editor"))
		srv.PATCH("/users/"+id, map[string]any{})
		// Re-fetch and verify all fields survived
		data := srv.GET("/users/" + id).Data()
		testutil.AssertEqual(t, "name intact", testutil.Field(t, data, "name"), "Original")
		testutil.AssertEqual(t, "role intact", testutil.Field(t, data, "role"), "editor")
	})
}

// ── H5: Multiple Replace registrations — last one wins ───────────────────────

func TestHigh_ReplaceLastWins(t *testing.T) {
	t.Parallel()

	t.Run("last_replace_wins_first_never_called", func(t *testing.T) {
		t.Parallel()
		firstCalled := false
		secondCalled := false

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					firstCalled = true
					ctx.Abort(http.StatusTeapot, "FIRST", "first replace")
					return nil
				}, maniflex.AtPosition(maniflex.Replace))

				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					secondCalled = true
					return next() // pass through to default DB handler
				}, maniflex.AtPosition(maniflex.Replace))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		if firstCalled {
			t.Error("first Replace must be superseded; it must never be called")
		}
		if !secondCalled {
			t.Error("second Replace must be the active handler")
		}
	})

	t.Run("three_replaces_only_last_runs", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var called []string

		record := func(name string, passthru bool) maniflex.MiddlewareFunc {
			return func(ctx *maniflex.ServerContext, next func() error) error {
				mu.Lock()
				called = append(called, name)
				mu.Unlock()
				if passthru {
					return next()
				}
				ctx.Abort(http.StatusTeapot, name, name)
				return nil
			}
		}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(record("first", false), maniflex.AtPosition(maniflex.Replace))
				s.Pipeline.Service.Register(record("second", false), maniflex.AtPosition(maniflex.Replace))
				s.Pipeline.Service.Register(record("third", true), maniflex.AtPosition(maniflex.Replace))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		snapshot := make([]string, len(called))
		copy(snapshot, called)
		mu.Unlock()

		testutil.AssertEqual(t, "exactly one Replace ran", len(snapshot), 1)
		testutil.AssertEqual(t, "third Replace ran", snapshot[0], "third")
	})

	t.Run("before_and_after_hooks_still_run_alongside_replace", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var order []string

		hook := func(name string) maniflex.MiddlewareFunc {
			return func(ctx *maniflex.ServerContext, next func() error) error {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return next()
			}
		}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(hook("before"), maniflex.AtPosition(maniflex.Before))
				s.Pipeline.Service.Register(hook("replace"), maniflex.AtPosition(maniflex.Replace))
				s.Pipeline.Service.Register(hook("after"), maniflex.AtPosition(maniflex.After))
			},
		})

		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		snapshot := make([]string, len(order))
		copy(snapshot, order)
		mu.Unlock()

		testutil.AssertEqual(t, "three hooks ran", len(snapshot), 3)
		testutil.AssertEqual(t, "before first", snapshot[0], "before")
		testutil.AssertEqual(t, "replace second", snapshot[1], "replace")
		testutil.AssertEqual(t, "after third", snapshot[2], "after")
	})
}

// ── Test infrastructure ───────────────────────────────────────────────────────

// postJSON sends a JSON POST and returns the HTTP status code.
func postJSON(t *testing.T, url string, body map[string]any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("postJSON marshal: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b)) //nolint:noctx
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
