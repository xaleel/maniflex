package e2e

// 5.4 — Computed/virtual fields. Server.AddComputedField registers a derived
// field that is materialised in the Response step. Tested against
// LockedInvoice (which already has a number, status, amount). We add
// "double_amount" and verify it shows up on every read path.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"maniflex"
	"maniflex/tests/e2e/testutil"
)

func computedServer(t *testing.T, install func(s *maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{testutil.LockedInvoice{}},
		Middleware: func(s *maniflex.Server) {
			if install != nil {
				install(s)
			}
		},
	})
}

func TestComputedField_AppearsOnCreate(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "double_amount",
			func(_ context.Context, row map[string]any) (any, error) {
				// `amount` may arrive as int64 (SQLite scan) or float64 (JSON
				// round-trip); handle both.
				switch v := row["amount"].(type) {
				case int64:
					return v * 2, nil
				case int:
					return v * 2, nil
				case float64:
					return v * 2, nil
				}
				return nil, errors.New("unexpected amount type")
			})
	})

	resp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-CF-1", "status": "draft", "amount": 50,
	})
	resp.AssertStatus(http.StatusCreated)
	got := resp.Data()["double_amount"]
	if got == nil {
		t.Fatal("computed field missing from create response")
	}
	// JSON decodes numbers as float64.
	if f, _ := got.(float64); f != 100 {
		t.Errorf("double_amount: got %v, want 100", got)
	}
}

func TestComputedField_AppearsOnRead(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "label",
			func(_ context.Context, row map[string]any) (any, error) {
				return row["number"].(string) + " (" + row["status"].(string) + ")", nil
			})
	})

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-CF-2", "status": "draft", "amount": 10,
	})
	id := createResp.ID()

	readResp := srv.GET("/locked_invoices/"+id, nil)
	readResp.AssertStatus(http.StatusOK)
	if got, _ := readResp.Data()["label"].(string); got != "INV-CF-2 (draft)" {
		t.Errorf("label: got %q, want %q", got, "INV-CF-2 (draft)")
	}
}

func TestComputedField_AppearsOnList(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "tag",
			func(_ context.Context, row map[string]any) (any, error) {
				return "tag-" + row["number"].(string), nil
			})
	})
	for _, n := range []string{"L1", "L2", "L3"} {
		srv.POST("/locked_invoices", map[string]any{
			"number": n, "status": "draft", "amount": 1,
		}).AssertStatus(http.StatusCreated)
	}

	resp := srv.GET("/locked_invoices", nil)
	resp.AssertStatus(http.StatusOK)
	rows := resp.DataList()
	if len(rows) != 3 {
		t.Fatalf("rows: got %d, want 3", len(rows))
	}
	for _, r := range rows {
		m := r.(map[string]any)
		number := m["number"].(string)
		tag, _ := m["tag"].(string)
		want := "tag-" + number
		if tag != want {
			t.Errorf("row %q: tag %q, want %q", number, tag, want)
		}
	}
}

func TestComputedField_AppearsOnUpdate(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "is_settled",
			func(_ context.Context, row map[string]any) (any, error) {
				return row["status"].(string) == "posted", nil
			})
	})

	createResp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-CF-3", "status": "draft", "amount": 9,
	})
	id := createResp.ID()

	updateResp := srv.PATCH("/locked_invoices/"+id, map[string]any{"amount": 11})
	updateResp.AssertStatus(http.StatusOK)
	if got, _ := updateResp.Data()["is_settled"].(bool); got {
		t.Errorf("is_settled: got true on draft, want false")
	}
}

func TestComputedField_ErrorIsLoggedAndFieldOmitted(t *testing.T) {
	// A failing computed function must not 500 the request — the field is
	// simply absent from the row.
	t.Parallel()
	var calls int32
	srv := computedServer(t, func(s *maniflex.Server) {
		s.MustAddComputedField("LockedInvoice", "broken",
			func(_ context.Context, _ map[string]any) (any, error) {
				atomic.AddInt32(&calls, 1)
				return nil, errors.New("intentional failure")
			})
	})

	resp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-CF-4", "status": "draft", "amount": 1,
	})
	resp.AssertStatus(http.StatusCreated) // not 500
	if _, present := resp.Data()["broken"]; present {
		t.Errorf("failing computed field should be omitted, but key 'broken' is present")
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Error("computed function should have been invoked")
	}
}

func TestComputedField_CollisionRejected(t *testing.T) {
	t.Parallel()
	srv := computedServer(t, func(s *maniflex.Server) {
		// Direct collision with an existing field on the model.
		err := s.AddComputedField("LockedInvoice", "amount",
			func(_ context.Context, _ map[string]any) (any, error) { return 0, nil })
		if err == nil {
			t.Error("expected error for computed field colliding with real field")
		}
	})
	// Sanity: server still works.
	resp := srv.POST("/locked_invoices", map[string]any{
		"number": "INV-CF-5", "status": "draft", "amount": 1,
	})
	resp.AssertStatus(http.StatusCreated)
}

func TestComputedField_DuplicateRejected(t *testing.T) {
	t.Parallel()
	computedServer(t, func(s *maniflex.Server) {
		_ = s.AddComputedField("LockedInvoice", "extra",
			func(_ context.Context, _ map[string]any) (any, error) { return 1, nil })
		err := s.AddComputedField("LockedInvoice", "extra",
			func(_ context.Context, _ map[string]any) (any, error) { return 2, nil })
		if err == nil {
			t.Error("expected error for duplicate computed field name")
		}
	})
}

func TestComputedField_UnknownModelRejected(t *testing.T) {
	t.Parallel()
	computedServer(t, func(s *maniflex.Server) {
		err := s.AddComputedField("DoesNotExist", "x",
			func(_ context.Context, _ map[string]any) (any, error) { return 0, nil })
		if err == nil {
			t.Error("expected error for unknown model")
		}
	})
}
