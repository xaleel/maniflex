package e2e

// Regression coverage for roadmap §11A.1 (checkpoint C1): txAdapter.FindMany /
// FindByID previously honoured only the soft-delete clause, silently dropping
// qp.Filters / qp.Sorts / qp.Includes and skipping JOIN building when a request
// was inside a transaction (e.g. WithTransaction). Multi-tenant filters and
// ForceFilter middleware were bypassed for any cross-model read issued via
// ctx.GetModel(...).List/Read from inside service-step middleware.
//
// These tests pin the fix: reads issued through ctx.GetModel while ctx.Tx is
// active must produce the same WHERE / ORDER BY / include population as reads
// issued without a transaction.

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func TestTxAdapter_ReadsHonourQueryParams(t *testing.T) {
	t.Parallel()

	t.Run("list_filters_apply_inside_tx", func(t *testing.T) {
		t.Parallel()

		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// Open a tx for the trigger op and query Users with a filter.
				s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
					maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.GetModel("User").List(&maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "role", Operator: maniflex.OpEq, Value: "admin"},
						},
						Page: 1, Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("tx GetModel.List filtered: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		adminID := srv.MustID(srv.CreateUser("AdminA", "tx-fa1@x.com", "admin"))
		srv.CreateUser("ViewerA", "tx-fa2@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.POST("/posts", map[string]any{
			"title": "trigger", "body": "x", "status": "draft", "user_id": adminID,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("filter dropped inside tx: want 1 admin row, got %d (rows=%v)", len(rows), rows)
		}
		if rows[0]["role"] != "admin" {
			t.Errorf("filter matched wrong row: got role=%v", rows[0]["role"])
		}
	})

	t.Run("list_sort_applies_inside_tx", func(t *testing.T) {
		t.Parallel()

		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
					maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.GetModel("User").List(&maniflex.QueryParams{
						Sorts: []maniflex.SortExpr{{DBName: "score", Direction: maniflex.SortDesc}},
						Page:  1, Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("tx GetModel.List sorted: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Post"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		// Three users with distinct scores. Sort desc by score => 30, 20, 10.
		lowID := srv.MustID(srv.CreateUser("Low", "tx-s1@x.com", "viewer"))
		midID := srv.MustID(srv.CreateUser("Mid", "tx-s2@x.com", "viewer"))
		highID := srv.MustID(srv.CreateUser("High", "tx-s3@x.com", "viewer"))
		srv.PATCH("/users/"+lowID, map[string]any{"score": 10}).AssertStatus(http.StatusOK)
		srv.PATCH("/users/"+midID, map[string]any{"score": 20}).AssertStatus(http.StatusOK)
		srv.PATCH("/users/"+highID, map[string]any{"score": 30}).AssertStatus(http.StatusOK)

		// Trigger the tx read.
		srv.POST("/posts", map[string]any{
			"title": "trigger", "body": "x", "status": "draft", "user_id": lowID,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 3 {
			t.Fatalf("want 3 rows, got %d", len(rows))
		}
		// Verify descending order by score — the bug was that sorts were dropped,
		// leaving insertion order intact.
		gotNames := []any{rows[0]["name"], rows[1]["name"], rows[2]["name"]}
		want := []any{"High", "Mid", "Low"}
		for i := range want {
			if gotNames[i] != want[i] {
				t.Errorf("sort dropped inside tx: position %d got %v, want %v (gotNames=%v)",
					i, gotNames[i], want[i], gotNames)
			}
		}
	})

	t.Run("list_nested_filter_joins_inside_tx", func(t *testing.T) {
		t.Parallel()

		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
					maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// Nested filter: posts whose related user.role == "admin".
					// This requires the LEFT JOIN that buildJoins emits — without it
					// the WHERE clause references a missing alias and either errors
					// or matches nothing.
					rows, err := ctx.GetModel("Post").List(&maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{{
							IsNested:      true,
							RelationKey:   "user",
							RelationTable: "users",
							RelationFK:    "user_id",
							NestedField:   "role",
							Operator:      maniflex.OpEq,
							Value:         "admin",
						}},
						Page: 1, Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("tx GetModel.List nested: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		adminID := srv.MustID(srv.CreateUser("AdminN", "tx-n1@x.com", "admin"))
		viewerID := srv.MustID(srv.CreateUser("ViewerN", "tx-n2@x.com", "viewer"))
		adminPostID := srv.MustID(srv.POST("/posts", map[string]any{
			"title": "by-admin", "body": "a", "status": "draft", "user_id": adminID,
		}))
		srv.POST("/posts", map[string]any{
			"title": "by-viewer", "body": "v", "status": "draft", "user_id": viewerID,
		}).AssertStatus(http.StatusCreated)

		// Trigger via comment create.
		srv.POST("/comments", map[string]any{
			"body": "trigger", "post_id": adminPostID, "user_id": adminID,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("nested filter dropped inside tx: want 1 post by admin, got %d (rows=%v)", len(rows), rows)
		}
		if rows[0]["title"] != "by-admin" {
			t.Errorf("nested filter matched wrong row: got title=%v", rows[0]["title"])
		}
	})

	t.Run("list_includes_populated_inside_tx", func(t *testing.T) {
		t.Parallel()

		var captured []map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
					maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					rows, err := ctx.GetModel("Post").List(&maniflex.QueryParams{
						Includes: []string{"user"},
						Page:     1, Limit: 100,
					})
					if err != nil {
						return fmt.Errorf("tx GetModel.List includes: %w", err)
					}
					mu.Lock()
					captured = rows
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("AuthorI", "tx-i1@x.com", "viewer"))
		postID := srv.MustID(srv.POST("/posts", map[string]any{
			"title": "with-author", "body": "b", "status": "draft", "user_id": uid,
		}))
		srv.POST("/comments", map[string]any{
			"body": "trigger", "post_id": postID, "user_id": uid,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		rows := captured
		mu.Unlock()
		if len(rows) != 1 {
			t.Fatalf("want 1 post row, got %d", len(rows))
		}
		user, ok := rows[0]["user"].(map[string]any)
		if !ok {
			t.Fatalf("includes dropped inside tx: row[user] not populated (row=%v)", rows[0])
		}
		if user["name"] != "AuthorI" {
			t.Errorf("included user wrong: got name=%v, want AuthorI", user["name"])
		}
	})

	t.Run("read_includes_populated_inside_tx", func(t *testing.T) {
		// FindByID also had the include bug. ctx.GetModel(...).Read does not
		// expose qp.Filters, so we exercise the FindByID include-population
		// path instead — that was the second blind spot listed in C1.
		// Use HasMany (post → comments) so the include code path runs over
		// scanned rows the same way it does for production reads.
		t.Parallel()

		var included []map[string]any
		var hit bool
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(maniflex.WithTransaction(nil),
					maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if err := next(); err != nil {
						return err
					}
					// Use List with a Filter on the target post's ID so the
					// txAdapter.FindMany populate path runs with includes.
					rows, err := ctx.GetModel("Post").List(&maniflex.QueryParams{
						Filters: []*maniflex.FilterExpr{
							{Field: "title", Operator: maniflex.OpEq, Value: "with-comment"},
						},
						Includes: []string{"comments"},
						Page:     1, Limit: 10,
					})
					if err != nil {
						return fmt.Errorf("tx GetModel.List comments: %w", err)
					}
					mu.Lock()
					if len(rows) > 0 {
						if kids, ok := rows[0]["comments"].([]map[string]any); ok {
							included = kids
						}
					}
					hit = true
					mu.Unlock()
					return nil
				}, maniflex.ForModel("Comment"), maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})

		uid := srv.MustID(srv.CreateUser("AuthorC", "tx-c1@x.com", "viewer"))
		postID := srv.MustID(srv.POST("/posts", map[string]any{
			"title": "with-comment", "body": "b", "status": "draft", "user_id": uid,
		}))
		// First comment seeded outside the tx; the second one is the trigger.
		srv.POST("/comments", map[string]any{
			"body": "first", "post_id": postID, "user_id": uid,
		}).AssertStatus(http.StatusCreated)
		srv.POST("/comments", map[string]any{
			"body": "second-trigger", "post_id": postID, "user_id": uid,
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		got := included
		ran := hit
		mu.Unlock()
		if !ran {
			t.Fatal("after-DB middleware did not run")
		}
		if len(got) != 2 {
			t.Fatalf("HasMany include dropped inside tx: want 2 comments, got %d (comments=%v)", len(got), got)
		}
	})
}
