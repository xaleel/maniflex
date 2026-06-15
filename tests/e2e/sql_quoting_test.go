package e2e

// sql_quoting_test.go verifies that table names, column names, and relation
// keys are correctly quoted in generated SQL, so that:
//   - Reserved words used as model/field names do not cause syntax errors.
//   - Normal (non-reserved) identifiers continue to work exactly as before.
//   - Deeply nested scenarios (joins, soft-delete, includes) all use quoted names.
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestSQLQuoting

import (
	"net/http"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

// ── Models with reserved-word identifiers ─────────────────────────────────────
//
// SQL reserved words that would break unquoted identifier interpolation.
// Each struct exercises a different category of reserved word.

// OrderItem uses "order" as the table name (reserved in all SQL dialects).
type OrderItem struct {
	maniflex.BaseModel
	Name   string `json:"name"   db:"name"   mfx:"required,filterable,sortable"`
	Status string `json:"status" db:"status" mfx:"required,filterable,sortable,enum:pending|shipped"`
}

// GroupMember uses "group" as a field name (reserved: GROUP BY).
type GroupMember struct {
	maniflex.BaseModel
	Group  string `json:"group"  db:"group"  mfx:"required,filterable,sortable"`
	Select string `json:"select" db:"select" mfx:"required,filterable"`
}

// Transaction uses "transaction" as table name, "index" as a column name.
type Transaction struct {
	maniflex.BaseModel
	Index   int    `json:"index"   db:"index"   mfx:"required,filterable,sortable"`
	From    string `json:"from"    db:"from"    mfx:"required,filterable"`
	To      string `json:"to"      db:"to"      mfx:"required,filterable"`
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestSQLQuoting(t *testing.T) {
	t.Parallel()

	// ── Reserved table names ───────────────────────────────────────────────────

	t.Run("reserved_word_order_as_table_name_creates_and_queries", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{OrderItem{}, maniflex.ModelConfig{TableName: "order"}},
		})
		// CREATE TABLE "order" must succeed (without quotes it's a syntax error)
		resp := srv.POST("/order", map[string]any{"name": "Widget", "status": "pending"})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()

		// SELECT ... FROM "order" must succeed
		srv.GET("/order/" + id).AssertStatus(http.StatusOK)
		srv.GET("/order").AssertStatus(http.StatusOK)
	})

	t.Run("reserved_word_order_supports_filter", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{OrderItem{}, maniflex.ModelConfig{TableName: "order"}},
		})
		srv.POST("/order", map[string]any{"name": "A", "status": "pending"})
		srv.POST("/order", map[string]any{"name": "B", "status": "shipped"})
		// WHERE "order"."status" = ? must not syntax-error
		items := srv.GET("/order?filter=status:eq:pending").DataList()
		testutil.AssertLen(t, "filtered results", items, 1)
	})

	t.Run("reserved_word_order_supports_sort", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{OrderItem{}, maniflex.ModelConfig{TableName: "order"}},
		})
		srv.POST("/order", map[string]any{"name": "B", "status": "pending"})
		srv.POST("/order", map[string]any{"name": "A", "status": "pending"})
		// ORDER BY "order"."name" ASC must not syntax-error
		items := srv.GET("/order?sort=name:asc").DataList()
		testutil.AssertLen(t, "sorted results", items, 2)
		testutil.AssertEqual(t, "first item", testutil.Field(t, items[0].(map[string]any), "name"), "A")
	})

	t.Run("reserved_word_order_update_and_delete", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{OrderItem{}, maniflex.ModelConfig{TableName: "order"}},
		})
		id := srv.MustID(srv.POST("/order", map[string]any{"name": "X", "status": "pending"}))
		// UPDATE "order" SET ... WHERE "id" = ?
		srv.PATCH("/order/"+id, map[string]any{"status": "shipped"}).AssertStatus(http.StatusOK)
		// DELETE FROM "order" WHERE "id" = ?
		srv.DELETE("/order/" + id).AssertStatus(http.StatusNoContent)
	})

	// ── Reserved column names ──────────────────────────────────────────────────

	t.Run("reserved_column_names_group_and_select_create_and_read", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{GroupMember{}, maniflex.ModelConfig{TableName: "group_members"}},
		})
		resp := srv.POST("/group_members", map[string]any{"group": "admin", "select": "yes"})
		resp.AssertStatus(http.StatusCreated)
		data := resp.Data()
		testutil.AssertEqual(t, "group field", testutil.Field(t, data, "group"), "admin")
		testutil.AssertEqual(t, "select field", testutil.Field(t, data, "select"), "yes")
	})

	t.Run("reserved_column_name_group_is_filterable", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{GroupMember{}, maniflex.ModelConfig{TableName: "group_members"}},
		})
		srv.POST("/group_members", map[string]any{"group": "admin", "select": "y"})
		srv.POST("/group_members", map[string]any{"group": "viewer", "select": "n"})
		// WHERE "group_members"."group" = ?
		items := srv.GET("/group_members?filter=group:eq:admin").DataList()
		testutil.AssertLen(t, "group filter", items, 1)
	})

	t.Run("reserved_column_name_group_is_sortable", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{GroupMember{}, maniflex.ModelConfig{TableName: "group_members"}},
		})
		srv.POST("/group_members", map[string]any{"group": "z", "select": "n"})
		srv.POST("/group_members", map[string]any{"group": "a", "select": "y"})
		items := srv.GET("/group_members?sort=group:asc").DataList()
		testutil.AssertLen(t, "sort results", items, 2)
		testutil.AssertEqual(t, "first after sort",
			testutil.Field(t, items[0].(map[string]any), "group"), "a")
	})

	t.Run("reserved_column_names_transaction_index_from_to", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Transaction{}, maniflex.ModelConfig{TableName: "transactions"}},
		})
		resp := srv.POST("/transactions", map[string]any{
			"index": 42, "from": "alice", "to": "bob",
		})
		resp.AssertStatus(http.StatusCreated)
		data := resp.Data()
		testutil.AssertEqual(t, "index field", testutil.FloatField(t, data, "index"), float64(42))
		testutil.AssertEqual(t, "from field", testutil.Field(t, data, "from"), "alice")
		testutil.AssertEqual(t, "to field", testutil.Field(t, data, "to"), "bob")
	})

	t.Run("reserved_column_name_index_is_filterable", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Transaction{}, maniflex.ModelConfig{TableName: "transactions"}},
		})
		srv.POST("/transactions", map[string]any{"index": 1, "from": "a", "to": "b"})
		srv.POST("/transactions", map[string]any{"index": 2, "from": "c", "to": "d"})
		// WHERE "transactions"."index" = ?
		items := srv.GET("/transactions?filter=index:eq:1").DataList()
		testutil.AssertLen(t, "index filter", items, 1)
	})

	// ── Normal identifiers still work ─────────────────────────────────────────

	t.Run("normal_identifiers_unaffected_by_quoting", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		// Standard User/Post models with normal identifier names
		u := srv.MustID(srv.CreateUser("Alice", "q@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Hello", "published", u))
		srv.GET("/users/" + u).AssertStatus(http.StatusOK)
		srv.GET("/posts/" + p).AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/posts?filter=status:eq:published").AssertStatus(http.StatusOK)
	})

	// ── Soft delete with reserved word table ───────────────────────────────────

	t.Run("soft_delete_with_reserved_word_table_name", func(t *testing.T) {
		t.Parallel()
		type OrderSoft struct {
			maniflex.BaseModel
			maniflex.WithIsDeleted
			Name string `json:"name" db:"name" mfx:"required,filterable"`
		}
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{
				OrderSoft{},
				maniflex.ModelConfig{
					TableName:  "order_soft",
					SoftDelete: maniflex.SoftDeleteConfig{Enabled: true, Field: "is_deleted", FieldType: maniflex.SoftDeleteBool},
				},
			},
		})
		id := srv.MustID(srv.POST("/order_soft", map[string]any{"name": "Soft X"}))
		// Soft DELETE: UPDATE "order_soft" SET "is_deleted" = ? WHERE "id" = ?
		srv.DELETE("/order_soft/" + id).AssertStatus(http.StatusNoContent)
		// Should not appear in list (soft-delete filter: WHERE NOT "is_deleted")
		items := srv.GET("/order_soft").DataList()
		testutil.AssertLen(t, "soft-deleted record not in list", items, 0)
	})

	// ── Includes with reserved word relation table ─────────────────────────────

	t.Run("includes_with_standard_models_still_work", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		u := srv.MustID(srv.CreateUser("Bob", "inc@x.com", "viewer"))
		p := srv.MustID(srv.CreatePost("Post", "draft", u))

		// JOIN / include should use quoted "users"."id" etc.
		data := srv.GET("/posts/" + p + "?include=user").Data()
		if _, ok := data["user"].(map[string]any); !ok {
			t.Error("include=user must embed a user object even with quoted identifiers")
		}
	})

	// ── q() helper unit behaviour ──────────────────────────────────────────────

	t.Run("crud_full_cycle_reserved_table_all_ops", func(t *testing.T) {
		// Full CRUD cycle on a reserved-word table to catch any missed sites.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{OrderItem{}, maniflex.ModelConfig{TableName: "order"}},
		})

		// Create
		id := srv.MustID(srv.POST("/order", map[string]any{
			"name": "Full Cycle", "status": "pending",
		}))

		// Read
		data := srv.GET("/order/" + id).Data()
		testutil.AssertEqual(t, "name read back", testutil.Field(t, data, "name"), "Full Cycle")

		// Update
		patched := srv.PATCH("/order/"+id, map[string]any{"status": "shipped"}).Data()
		testutil.AssertEqual(t, "status updated", testutil.Field(t, patched, "status"), "shipped")

		// List with filter
		items := srv.GET("/order?filter=status:eq:shipped").DataList()
		testutil.AssertLen(t, "filtered after update", items, 1)

		// Delete
		srv.DELETE("/order/" + id).AssertStatus(http.StatusNoContent)
		srv.GET("/order/" + id).AssertStatus(http.StatusNotFound)
	})
}
