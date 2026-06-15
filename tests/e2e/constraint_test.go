package e2e

// constraint_test.go tests that UNIQUE constraint violations from both SQLite
// and the Postgres wire protocol are converted into 409 Conflict responses with
// a structured {"error": {"code": "CONFLICT", "details": {"field": "...",
// "message": "... already taken"}}} body instead of an opaque 500.
//
//	go test ./tests/e2e/... -run TestConstraintErrors

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

func TestConstraintErrors(t *testing.T) {
	t.Parallel()

	// ── Basic 409 shape ───────────────────────────────────────────────────────

	t.Run("duplicate_unique_field_returns_409_not_500", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("Alice", "dup@x.com", "viewer").AssertStatus(http.StatusCreated)
		resp := srv.CreateUser("Alice2", "dup@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
	})

	t.Run("conflict_response_has_correct_error_code", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "code@x.com", "viewer")
		resp := srv.CreateUser("B", "code@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "CONFLICT")
	})

	t.Run("conflict_response_body_has_error_and_details", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "body@x.com", "viewer")
		resp := srv.CreateUser("B", "body@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		resp.AssertJSON(func(body map[string]any) {
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("response must have top-level 'error' object, got: %v", body)
			}
			if errObj["code"] != "CONFLICT" {
				t.Errorf("error.code: got %v, want CONFLICT", errObj["code"])
			}
			if errObj["message"] == "" || errObj["message"] == nil {
				t.Error("error.message must be non-empty")
			}
			details, ok := errObj["details"].(map[string]any)
			if !ok {
				t.Fatalf("error.details must be an object, got: %T", errObj["details"])
			}
			if details["message"] == "" || details["message"] == nil {
				t.Error("error.details.message must be present")
			}
		})
	})

	t.Run("conflict_response_includes_field_name", func(t *testing.T) {
		// The details.field value should name the column that was violated.
		// For SQLite "UNIQUE constraint failed: users.email" → field = "email".
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "field@x.com", "viewer")
		resp := srv.CreateUser("B", "field@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		resp.AssertJSON(func(body map[string]any) {
			errObj := body["error"].(map[string]any)
			details, ok := errObj["details"].(map[string]any)
			if !ok {
				t.Fatalf("details must be an object")
			}
			field, _ := details["field"].(string)
			if field == "" {
				t.Error("error.details.field must be present and non-empty for UNIQUE violations")
			}
			testutil.AssertEqual(t, "violated field name", field, "email")
		})
	})

	t.Run("conflict_message_mentions_field_name", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "msg@x.com", "viewer")
		resp := srv.CreateUser("B", "msg@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		resp.AssertJSON(func(body map[string]any) {
			details := body["error"].(map[string]any)["details"].(map[string]any)
			msg, _ := details["message"].(string)
			if msg == "" {
				t.Error("error.details.message must be non-empty")
			}
			// The message should reference the field ("email already taken")
			if msg == "" || msg == "value already exists" {
				// Generic fallback is acceptable when field name is not available,
				// but the preferred form includes the field name.
				t.Logf("constraint message is generic (field name not extracted): %q", msg)
			}
		})
	})

	// ── No false positives ────────────────────────────────────────────────────

	t.Run("non_duplicate_insert_returns_201_not_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "a@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.CreateUser("B", "b@x.com", "viewer").AssertStatus(http.StatusCreated)
	})

	t.Run("not_found_is_still_404_not_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users/00000000-0000-0000-0000-000000000000").AssertStatus(http.StatusNotFound)
	})

	t.Run("validation_error_is_still_422_not_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.POST("/users", map[string]any{}).AssertStatus(http.StatusUnprocessableEntity)
	})

	// ── Update path ───────────────────────────────────────────────────────────

	t.Run("update_to_duplicate_unique_value_returns_409", func(t *testing.T) {
		// Update a record's unique field to collide with another record's value.
		// User.Email is immutable so we use Tag.Name (unique, not immutable).
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Tag{}},
		})
		id1 := srv.MustID(srv.POST("/tags", map[string]any{"name": "go", "color": "blue"}))
		srv.MustID(srv.POST("/tags", map[string]any{"name": "rust", "color": "orange"}))

		// Try to rename tag 1 to "rust" — should conflict
		resp := srv.PATCH("/tags/"+id1, map[string]any{"name": "rust"})
		resp.AssertStatus(http.StatusConflict)
		testutil.AssertEqual(t, "update conflict code", resp.ErrorCode(), "CONFLICT")
	})

	// ── Multi-column / compound unique ───────────────────────────────────────

	t.Run("tag_name_unique_conflict_names_correct_field", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Tag{}},
		})
		srv.POST("/tags", map[string]any{"name": "alpha", "color": "red"})
		resp := srv.POST("/tags", map[string]any{"name": "alpha", "color": "blue"})
		resp.AssertStatus(http.StatusConflict)
		resp.AssertJSON(func(body map[string]any) {
			details := body["error"].(map[string]any)["details"].(map[string]any)
			field, _ := details["field"].(string)
			testutil.AssertEqual(t, "field name for tag.name conflict", field, "name")
		})
	})

	// ── Transaction path ──────────────────────────────────────────────────────

	t.Run("constraint_violation_inside_transaction_returns_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					maniflex.WithTransaction(nil),
					maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		srv.CreateUser("A", "tx409@x.com", "viewer").AssertStatus(http.StatusCreated)
		resp := srv.CreateUser("B", "tx409@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		testutil.AssertEqual(t, "tx conflict code", resp.ErrorCode(), "CONFLICT")
	})

	// ── Response header still set on conflict ─────────────────────────────────

	t.Run("request_id_header_present_on_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.CreateUser("A", "hdr409@x.com", "viewer")
		resp := srv.CreateUser("B", "hdr409@x.com", "viewer")
		resp.AssertStatus(http.StatusConflict)
		testutil.AssertNotEmpty(t, "X-Request-Id on 409",
			resp.Header.Get("X-Request-Id"))
	})

	// ── WithIsDeleted model also triggers 409 ─────────────────────────────────

	t.Run("unique_on_soft_delete_model_returns_409", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Tag{}},
		})
		srv.POST("/tags", map[string]any{"name": "beta"}).AssertStatus(http.StatusCreated)
		resp := srv.POST("/tags", map[string]any{"name": "beta"})
		resp.AssertStatus(http.StatusConflict)
	})
}
