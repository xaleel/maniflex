package e2e_test

// 13.9 — a scoped singleton's audit diff.
//
// A Singleton has no {id} in the URL, so the handler pins ctx.ResourceID to the
// SingletonID placeholder and the DB *step* later swaps in the real row id. But
// db.AuditLog(WithChanges()) registers on the DB *pipeline* at Before position,
// so its pre-fetch runs while ResourceID is still the placeholder. The read
// misses, `before` is nil, and changesForUpdate then reports every field in the
// result as From=nil — not a missing diff, a false one claiming the caller set
// fields it never touched.

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// AuditSite is a one-row-per-owner storefront, the scoped-singleton shape.
type AuditSite struct {
	maniflex.BaseModel
	OwnerID string `json:"owner_id" db:"owner_id" mfx:"filterable,unique,default:"`
	Banner  string `json:"banner"   db:"banner"   mfx:"default:untitled"`
	Theme   string `json:"theme"    db:"theme"    mfx:"enum:light|dark,default:light"`
}

// auditSingletonSrv mounts AuditSite as a singleton scoped to X-Owner, with
// change-tracking audit at the default (Before) position — the placement the
// WithChanges godoc tells you to use.
func auditSingletonSrv(t *testing.T, sink *memWriteAuditSink, scoped bool) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{AuditSite{}, maniflex.ModelConfig{Singleton: true, TableName: "audit_sites"}},
		Middleware: func(s *maniflex.Server) {
			if scoped {
				s.Pipeline.DB.Register(
					dbmw.ForceFilter("owner_id", func(ctx *maniflex.ServerContext) any {
						if o := ctx.Request.Header.Get("X-Owner"); o != "" {
							return o
						}
						return nil
					}), maniflex.ForModel("AuditSite"))
			}
			s.Pipeline.DB.Register(
				dbmw.AuditLog(sink, dbmw.WithChanges()),
				maniflex.ForModel("AuditSite"),
				maniflex.ForOperation(maniflex.OpUpdate),
			)
		},
	})
}

// TestAuditLog_ScopedSingletonDiff is the bug: the PATCH must be audited against
// the row it actually edited.
func TestAuditLog_ScopedSingletonDiff(t *testing.T) {
	sink := &memWriteAuditSink{}
	srv := auditSingletonSrv(t, sink, true)
	owner := map[string]string{"X-Owner": "owner-a"}

	// Provision the row, and confirm the state we are about to diff against.
	got := srv.GET("/audit_sites", owner).AssertStatus(http.StatusOK).Data()
	if got["banner"] != "untitled" {
		t.Fatalf("setup: banner = %v, want the default %q", got["banner"], "untitled")
	}
	realID, _ := got["id"].(string)

	srv.PATCH("/audit_sites", map[string]any{"banner": "spring sale"}, owner).
		AssertStatus(http.StatusOK)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("no audit record written")
	}
	if rec.ResourceID != realID {
		t.Errorf("ResourceID = %q, want the resolved row id %q", rec.ResourceID, realID)
	}

	banner, ok := rec.Changes["banner"]
	if !ok {
		t.Fatalf("banner is missing from the diff; got %v", rec.Changes)
	}
	if banner.From != "untitled" || banner.To != "spring sale" {
		t.Errorf("banner: From=%v To=%v, want From=%q To=%q",
			banner.From, banner.To, "untitled", "spring sale")
	}

	// The anti-over-reach half, and the sharper symptom: with no prior state
	// every field reads as newly set, so an untouched field is reported as an
	// edit the caller never made.
	if theme, reported := rec.Changes["theme"]; reported {
		t.Errorf("theme was not touched but is reported as changed (From=%v To=%v) — "+
			"the diff is against nothing, not against the row", theme.From, theme.To)
	}
}

// TestAuditLog_GlobalSingletonDiff pins the unscoped shape too. Its ResourceID
// placeholder *is* the real id, so this one should already pass — it is here so a
// fix aimed at the scoped case cannot quietly break it.
func TestAuditLog_GlobalSingletonDiff(t *testing.T) {
	sink := &memWriteAuditSink{}
	srv := auditSingletonSrv(t, sink, false)

	srv.GET("/audit_sites").AssertStatus(http.StatusOK)
	srv.PATCH("/audit_sites", map[string]any{"banner": "spring sale"}).
		AssertStatus(http.StatusOK)
	time.Sleep(30 * time.Millisecond)

	rec, ok := sink.last()
	if !ok {
		t.Fatal("no audit record written")
	}
	if rec.ResourceID != maniflex.SingletonID {
		t.Errorf("ResourceID = %q, want %q", rec.ResourceID, maniflex.SingletonID)
	}
	banner, ok := rec.Changes["banner"]
	if !ok {
		t.Fatalf("banner is missing from the diff; got %v", rec.Changes)
	}
	if banner.From != "untitled" {
		t.Errorf("banner.From = %v, want %q", banner.From, "untitled")
	}
	if _, reported := rec.Changes["theme"]; reported {
		t.Error("theme was not touched but is reported as changed")
	}
}
