package e2e

import (
	"net/http"
	"testing"

	"maniflex/tests/e2e/testutil"
)

// TestValidation covers the Deserialize and Validate pipeline steps.
func TestValidation(t *testing.T) {
	t.Parallel()

	// ── Deserialization ───────────────────────────────────────────────────────

	t.Run("empty_body_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.Do("POST", srv.APIPath("/users"), []byte{})
		resp.AssertStatus(http.StatusBadRequest)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "EMPTY_BODY")
	})

	t.Run("malformed_json_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.Do("POST", srv.APIPath("/users"), []byte("{not valid json"))
		resp.AssertStatus(http.StatusBadRequest)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "INVALID_JSON")
	})

	t.Run("array_body_instead_of_object_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.Do("POST", srv.APIPath("/users"), []byte("[1,2,3]"))
		resp.AssertStatus(http.StatusBadRequest)
	})

	// ── Required fields ───────────────────────────────────────────────────────

	t.Run("missing_required_field_returns_422", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// name is required but omitted
		resp := srv.POST("/users", map[string]any{
			"email": "a@x.com", "password": "s", "role": "viewer",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "VALIDATION_ERROR")
	})

	t.Run("missing_multiple_required_fields_all_reported", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// name, email, password all required
		resp := srv.POST("/users", map[string]any{"role": "viewer"})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		resp.AssertJSON(func(body map[string]any) {
			errObj := body["error"].(map[string]any)
			details, ok := errObj["details"].([]any)
			if !ok || len(details) < 3 {
				t.Errorf("expected ≥3 validation errors, got details: %v", errObj["details"])
			}
		})
	})

	t.Run("null_required_field_returns_422", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": nil, "email": "a@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("required_field_present_on_update_not_enforced", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Alice", "alice@x.com", "viewer"))
		// PATCH omitting "name" (required on create) must succeed on update
		resp := srv.PATCH("/users/"+id, map[string]any{"role": "editor"})
		resp.AssertStatus(http.StatusOK)
	})

	// ── Enum validation ───────────────────────────────────────────────────────

	t.Run("invalid_enum_value_returns_422", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "A", "email": "a@x.com", "password": "s",
			"role": "superadmin", // not in enum:admin|editor|viewer
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "VALIDATION_ERROR")
	})

	t.Run("valid_enum_value_passes", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for _, role := range []string{"admin", "editor", "viewer"} {
			email := role + "@x.com"
			resp := srv.POST("/users", map[string]any{
				"name": role, "email": email, "password": "s", "role": role,
			})
			resp.AssertStatus(http.StatusCreated)
		}
	})

	t.Run("absent_enum_field_skips_validation", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// role has an enum but is not required; omitting it is fine
		resp := srv.POST("/users", map[string]any{
			"name": "NoRole", "email": "norole@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
	})

	t.Run("invalid_enum_on_update_returns_422", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("A", "a2@x.com", "viewer"))
		resp := srv.PATCH("/users/"+id, map[string]any{"role": "god"})
		resp.AssertStatus(http.StatusUnprocessableEntity)
	})

	// ── Readonly / immutable stripping ────────────────────────────────────────

	t.Run("supplying_id_on_create_is_ignored", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"id": "custom-id", "name": "A", "email": "a3@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
		if id := resp.ID(); id == "custom-id" {
			t.Error("client-supplied id must be ignored; adapter generates the id")
		}
	})

	t.Run("supplying_created_at_on_create_is_ignored", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "A", "email": "a4@x.com", "password": "s",
			"created_at": "2000-01-01T00:00:00Z", // readonly
		})
		resp.AssertStatus(http.StatusCreated)
		createdAt := testutil.Field(t, resp.Data(), "created_at")
		if createdAt == "2000-01-01T00:00:00Z" {
			t.Error("readonly created_at must not be set from client data")
		}
	})

	t.Run("immutable_field_can_be_set_on_create", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"name": "A", "email": "immutable@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "email set on create",
			testutil.Field(t, resp.Data(), "email"), "immutable@x.com")
	})

	t.Run("immutable_field_silently_ignored_on_update", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("A", "keep@x.com", "viewer"))
		srv.PATCH("/users/"+id, map[string]any{"email": "changed@x.com", "name": "B"})
		got := testutil.Field(t, srv.GET("/users/"+id).Data(), "email")
		testutil.AssertEqual(t, "email unchanged", got, "keep@x.com")
	})

	// ── Query param validation ────────────────────────────────────────────────

	t.Run("invalid_page_param_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users?page=abc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("page_zero_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users?page=0").AssertStatus(http.StatusBadRequest)
	})

	t.Run("invalid_limit_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users?limit=notanumber").AssertStatus(http.StatusBadRequest)
	})

	t.Run("sort_on_non_sortable_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// body is not sortable
		srv.GET("/posts?sort=body:asc").AssertStatus(http.StatusBadRequest)
	})

	t.Run("include_nonexistent_relation_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users?include=nonexistent").AssertStatus(http.StatusBadRequest)
	})

	t.Run("filter_on_non_filterable_field_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// body field has no mfx:"filterable" tag
		srv.GET("/posts?filter=body:eq:hello").AssertStatus(http.StatusBadRequest)
	})

	t.Run("filter_unknown_operator_returns_400", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		srv.GET("/users?filter=name:fuzzy:x").AssertStatus(http.StatusBadRequest)
	})
}
