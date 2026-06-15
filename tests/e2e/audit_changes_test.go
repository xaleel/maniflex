package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"maniflex"
	dbmw "maniflex/middleware/db"
	"maniflex/tests/e2e/testutil"
)

// memWriteAuditSink captures AuditRecords for test assertions.
type memWriteAuditSink struct {
	mu      sync.Mutex
	records []dbmw.AuditRecord
}

func (s *memWriteAuditSink) Write(_ context.Context, r dbmw.AuditRecord) error {
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return nil
}

func (s *memWriteAuditSink) last() (dbmw.AuditRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return dbmw.AuditRecord{}, false
	}
	return s.records[len(s.records)-1], true
}

func (s *memWriteAuditSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// AuditableItem is a model with two auditable fields.
type AuditableItem struct {
	maniflex.BaseModel
	Title  string `json:"title"  db:"title"  mfx:"required,filterable"`
	Status string `json:"status" db:"status" mfx:"required,enum:draft|active|closed"`
	Secret string `json:"secret" db:"secret" mfx:"filterable"`
}

func TestAuditLog_TenantID(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				ctx.Auth = &maniflex.AuthInfo{UserID: "u1", TenantID: "tenant-abc"}
				return next()
			})
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	s.POST("/auditable_items", map[string]any{"title": "T", "status": "draft", "secret": "x"}).
		AssertStatus(http.StatusCreated)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record")
	}
	if rec.TenantID != "tenant-abc" {
		t.Errorf("tenant_id: got %q, want %q", rec.TenantID, "tenant-abc")
	}
}

func TestAuditLog_NoTenantIDWhenAuthNil(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	s.POST("/auditable_items", map[string]any{"title": "T", "status": "draft", "secret": "x"}).
		AssertStatus(http.StatusCreated)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record")
	}
	if rec.TenantID != "" {
		t.Errorf("tenant_id must be empty when auth is nil, got %q", rec.TenantID)
	}
}

func TestAuditLog_WithChanges_Create(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges()),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	s.POST("/auditable_items", map[string]any{"title": "Hello", "status": "draft", "secret": "s"}).
		AssertStatus(http.StatusCreated)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record")
	}
	if len(rec.Changes) == 0 {
		t.Fatal("create must produce Changes")
	}
	// Every field in Changes for create must have From=nil
	for field, fc := range rec.Changes {
		if fc.From != nil {
			t.Errorf("create Changes[%s].From must be nil, got %v", field, fc.From)
		}
		if fc.To == nil {
			t.Errorf("create Changes[%s].To must not be nil", field)
		}
	}
	// Title must appear
	if _, ok := rec.Changes["title"]; !ok {
		t.Error("Changes must include 'title' field")
	}
}

func TestAuditLog_WithChanges_Update_OnlyChangedFields(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges()),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
			)
		},
	})

	id := s.MustID(s.POST("/auditable_items", map[string]any{
		"title": "Original", "status": "draft", "secret": "s",
	}))
	time.Sleep(10 * time.Millisecond)
	sink.mu.Lock()
	sink.records = sink.records[:0] // reset after create
	sink.mu.Unlock()

	s.PATCH("/auditable_items/"+id, map[string]any{"status": "active"}).
		AssertStatus(http.StatusOK)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record for update")
	}
	if len(rec.Changes) == 0 {
		t.Fatal("update must produce Changes")
	}
	// Only status changed
	if _, hasStatus := rec.Changes["status"]; !hasStatus {
		t.Error("Changes must include 'status'")
	}
	// Title was not changed — must not appear
	if _, hasTitle := rec.Changes["title"]; hasTitle {
		t.Error("Changes must not include unchanged field 'title'")
	}
	// status change: from draft → active
	sc := rec.Changes["status"]
	if fmt.Sprint(sc.From) != "draft" {
		t.Errorf("status.From: got %v, want draft", sc.From)
	}
	if fmt.Sprint(sc.To) != "active" {
		t.Errorf("status.To: got %v, want active", sc.To)
	}
}

func TestAuditLog_WithChanges_Delete(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges()),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpDelete),
			)
		},
	})

	id := s.MustID(s.POST("/auditable_items", map[string]any{
		"title": "ToDelete", "status": "draft", "secret": "s",
	}))
	time.Sleep(10 * time.Millisecond)
	sink.mu.Lock()
	sink.records = sink.records[:0]
	sink.mu.Unlock()

	s.DELETE("/auditable_items/" + id).AssertStatus(http.StatusNoContent)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record for delete")
	}
	if len(rec.Changes) == 0 {
		t.Fatal("delete must produce Changes")
	}
	// Every field in Changes for delete must have To=nil
	for field, fc := range rec.Changes {
		if fc.To != nil {
			t.Errorf("delete Changes[%s].To must be nil, got %v", field, fc.To)
		}
		if fc.From == nil {
			t.Errorf("delete Changes[%s].From must not be nil", field)
		}
	}
	if _, ok := rec.Changes["title"]; !ok {
		t.Error("Changes must include 'title' for delete")
	}
}

func TestAuditLog_WithExcludeFields(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges(), dbmw.WithExcludeFields("secret")),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate),
			)
		},
	})

	s.POST("/auditable_items", map[string]any{"title": "T", "status": "draft", "secret": "mysecret"}).
		AssertStatus(http.StatusCreated)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record")
	}
	if _, found := rec.Changes["secret"]; found {
		t.Error("excluded field 'secret' must not appear in Changes")
	}
	if _, found := rec.Changes["title"]; !found {
		t.Error("non-excluded field 'title' must appear in Changes")
	}
}

func TestAuditLog_NoChangesWithoutOption(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink), // no WithChanges
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpCreate),
				maniflex.AtPosition(maniflex.After),
			)
		},
	})

	s.POST("/auditable_items", map[string]any{"title": "T", "status": "draft", "secret": "s"}).
		AssertStatus(http.StatusCreated)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("expected an audit record")
	}
	if rec.Changes != nil {
		t.Error("Changes must be nil when WithChanges is not used")
	}
}

func TestAuditLog_NoFireOn4xx(t *testing.T) {
	sink := &memWriteAuditSink{}

	s := testutil.NewServer(t, testutil.Options{
		Models: []any{AuditableItem{}},
		Middleware: func(srv *maniflex.Server) {
			srv.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges()),
				maniflex.ForModel("AuditableItem"),
				maniflex.ForOperation(maniflex.OpUpdate),
			)
		},
	})

	s.PATCH("/auditable_items/00000000-0000-0000-0000-000000000000", map[string]any{
		"status": "active",
	}).AssertStatus(http.StatusNotFound)
	time.Sleep(30 * time.Millisecond)

	if n := sink.count(); n != 0 {
		t.Errorf("must not write audit record for 4xx, got %d", n)
	}
}
