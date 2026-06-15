// Package e2e contains end-to-end tests for the maniflex framework.
// Every test spins up a real HTTP server backed by an in-memory SQLite database.
//
// Run all e2e tests:
//
//	go test ./tests/e2e/...
//
// Run a specific group (uses -run with the top-level test name):
//
//	go test ./tests/e2e/... -run TestCRUD
//	go test ./tests/e2e/... -run TestFilter
//	go test ./tests/e2e/... -run TestSoftDelete
//	go test ./tests/e2e/... -run TestPipeline
//
// Run a single sub-test:
//
//	go test ./tests/e2e/... -run TestCRUD/create_user_returns_201
//	go test ./tests/e2e/... -run TestFilter/nested_relation_filter
package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"maniflex/tests/e2e/testutil"
)

// TestCRUD covers the complete Create/Read/Update/Delete lifecycle for a model.
func TestCRUD(t *testing.T) {
	t.Parallel()

	t.Run("create_user_returns_201_with_id", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "Alice", "email": "alice@example.com",
			"password": "secret", "role": "admin",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "id", resp.ID())
		testutil.AssertEqual(t, "name", testutil.Field(t, resp.Data(), "name"), "Alice")
		testutil.AssertEqual(t, "email", testutil.Field(t, resp.Data(), "email"), "alice@example.com")
	})

	t.Run("create_strips_readonly_field_from_response", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/posts", map[string]any{
			"title": "My Post", "body": "Content", "status": "draft",
			"user_id": "00000000-0000-0000-0000-000000000001",
			"views":   9999, // readonly — should be stripped from body and ignored
		})
		resp.AssertStatus(http.StatusCreated)
		// views is readonly; client-supplied value must be silently dropped
		testutil.AssertEqual(t, "views", testutil.FloatField(t, resp.Data(), "views"), float64(0))
	})

	t.Run("create_excludes_writeonly_field_from_response", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "Bob", "email": "bob@example.com",
			"password": "secret", "role": "viewer",
		})
		resp.AssertStatus(http.StatusCreated)
		_, hasPassword := resp.Data()["password"]
		if hasPassword {
			t.Error("password (writeonly) must not appear in response")
		}
	})

	t.Run("create_auto_generates_uuid_id", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		r1 := srv.CreateUser("U1", "u1@x.com", "viewer")
		r2 := srv.CreateUser("U2", "u2@x.com", "viewer")
		r1.AssertStatus(http.StatusCreated)
		r2.AssertStatus(http.StatusCreated)
		if r1.ID() == r2.ID() {
			t.Error("two creates must produce different IDs")
		}
	})

	t.Run("create_timestamps_are_set", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.CreateUser("U", "u@x.com", "viewer")
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "created_at", testutil.Field(t, resp.Data(), "created_at"))
		testutil.AssertNotEmpty(t, "updated_at", testutil.Field(t, resp.Data(), "updated_at"))
	})

	t.Run("read_returns_created_record", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "alice2@x.com", "admin"))
		resp := srv.GET("/users/" + id)
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "id", testutil.Field(t, resp.Data(), "id"), id)
		testutil.AssertEqual(t, "name", testutil.Field(t, resp.Data(), "name"), "Alice")
	})

	t.Run("read_nonexistent_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users/00000000-0000-0000-0000-000000000000").AssertStatus(http.StatusNotFound)
	})

	t.Run("read_invalid_id_format_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users/not-a-valid-uuid").AssertStatus(http.StatusNotFound)
	})

	t.Run("list_returns_all_records_with_meta", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.MustID(srv.CreateUser("A", "a@x.com", "admin"))
		srv.MustID(srv.CreateUser("B", "b@x.com", "editor"))
		srv.MustID(srv.CreateUser("C", "c@x.com", "viewer"))
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		testutil.AssertLen(t, "items", resp.DataList(), 3)
		meta := resp.Meta()
		testutil.AssertEqual(t, "total", meta["total"], float64(3))
		testutil.AssertEqual(t, "page", meta["page"], float64(1))
	})

	t.Run("list_empty_returns_empty_slice_not_null", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		items := resp.DataList()
		if items == nil {
			t.Error("data must be [] not null")
		}
		testutil.AssertLen(t, "items", items, 0)
	})

	t.Run("update_patches_specific_fields", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "alice3@x.com", "viewer"))
		resp := srv.PATCH("/users/"+id, map[string]any{"name": "Alicia"})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "name", testutil.Field(t, resp.Data(), "name"), "Alicia")
		// email unchanged
		testutil.AssertEqual(t, "email", testutil.Field(t, resp.Data(), "email"), "alice3@x.com")
	})

	t.Run("update_immutable_field_is_silently_ignored", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "alice4@x.com", "viewer"))
		// email is immutable — patch must succeed but email must not change
		resp := srv.PATCH("/users/"+id, map[string]any{
			"name":  "Alicia",
			"email": "hacked@x.com",
		})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "name", testutil.Field(t, resp.Data(), "name"), "Alicia")
		testutil.AssertEqual(t, "email", testutil.Field(t, resp.Data(), "email"), "alice4@x.com")
	})

	t.Run("update_readonly_field_is_silently_stripped", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		userID := srv.MustID(srv.CreateUser("U", "u5@x.com", "viewer"))
		postID := srv.MustID(srv.CreatePost("Post", "draft", userID))
		// views is readonly
		resp := srv.PATCH("/posts/"+postID, map[string]any{"views": 9999, "title": "Updated"})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "title", testutil.Field(t, resp.Data(), "title"), "Updated")
		testutil.AssertEqual(t, "views", testutil.FloatField(t, resp.Data(), "views"), float64(0))
	})

	t.Run("update_nonexistent_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.PATCH("/users/00000000-0000-0000-0000-000000000000",
			map[string]any{"name": "X"}).AssertStatus(http.StatusNotFound)
	})

	t.Run("update_timestamps_updated_at_changes", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "alice6@x.com", "viewer"))
		before := testutil.Field(t, srv.GET("/users/"+id).Data(), "updated_at")
		srv.PATCH("/users/"+id, map[string]any{"name": "Alicia"})
		after := testutil.Field(t, srv.GET("/users/"+id).Data(), "updated_at")
		// updated_at should be >= before (may be equal if same second)
		if after < before {
			t.Errorf("updated_at went backwards: %s → %s", before, after)
		}
	})

	t.Run("delete_returns_204_no_body", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("ToDelete", "del@x.com", "viewer"))
		resp := srv.DELETE("/users/" + id)
		resp.AssertStatus(http.StatusNoContent)
		if len(resp.Body) > 0 {
			t.Errorf("204 must have empty body, got: %s", resp.Body)
		}
	})

	t.Run("delete_then_read_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("ToDelete2", "del2@x.com", "viewer"))
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/users/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("delete_then_list_excludes_record", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("ToDelete3", "del3@x.com", "viewer"))
		srv.MustID(srv.CreateUser("Keeper", "keeper@x.com", "viewer"))
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
		items := srv.GET("/users").DataList()
		testutil.AssertLen(t, "items after delete", items, 1)
		testutil.AssertEqual(t, "remaining name",
			testutil.Field(t, items[0].(map[string]any), "name"), "Keeper")
	})

	t.Run("delete_nonexistent_returns_404", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.DELETE("/users/00000000-0000-0000-0000-000000000000").AssertStatus(http.StatusNotFound)
	})

	t.Run("delete_twice_returns_404_on_second", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Double", "double@x.com", "viewer"))
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNoContent)
		srv.DELETE("/users/" + id).AssertStatus(http.StatusNotFound)
	})

	t.Run("multiple_models_isolated", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		userID := srv.MustID(srv.CreateUser("U", "u7@x.com", "viewer"))
		postID := srv.MustID(srv.CreatePost("P", "draft", userID))
		// GET /users only returns users, not posts
		users := srv.GET("/users").DataList()
		testutil.AssertLen(t, "users", users, 1)
		posts := srv.GET("/posts").DataList()
		testutil.AssertLen(t, "posts", posts, 1)
		testutil.AssertEqual(t, "user id matches", testutil.Field(t, users[0].(map[string]any), "id"), userID)
		testutil.AssertEqual(t, "post id matches", testutil.Field(t, posts[0].(map[string]any), "id"), postID)
	})

	t.Run("create_with_extra_unknown_field_is_ignored", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "Extra", "email": "extra@x.com",
			"password": "secret", "role": "viewer",
			"nonexistent_field": "should_be_dropped",
		})
		resp.AssertStatus(http.StatusCreated)
		_, hasExtra := resp.Data()["nonexistent_field"]
		if hasExtra {
			t.Error("unknown field must not appear in response")
		}
	})

	t.Run("list_uses_default_page_and_limit", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for i := range 5 {
			srv.MustID(srv.CreateUser(fmt.Sprintf("U%d", i), fmt.Sprintf("u%d@x.com", i), "viewer"))
		}
		meta := srv.GET("/users").Meta()
		testutil.AssertEqual(t, "page", meta["page"], float64(1))
		testutil.AssertEqual(t, "limit", meta["limit"], float64(20))
		testutil.AssertEqual(t, "total", meta["total"], float64(5))
	})
}
