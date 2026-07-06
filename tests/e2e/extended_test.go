package e2e

// extended_test.go covers the medium and lower-priority gaps identified after
// auditing the initial e2e suite. Nothing in this file touches any other test file.
//
// Medium (6 groups):
//   M6  - response.Logging and response.Envelope
//   M7  - service.SlugifyField, StripField, CopyField, TimestampWhen, OwnerScope, Emit, Webhook
//   M8  - validate.UniqueField self-exclusion on update
//   M9  - validate.CrossFieldValidate, validate.RegexField
//   M10 - db.RateLimit, db.Paginate, db.Tenancy, db.Invalidate
//   M11 - auth.RequireOwner, auth.AllowPublicRead
//
// Lower priority (6 groups):
//   L12 - Custom ModelConfig.TableName
//   L13 - PathPrefix customisation
//   L14 - GET /health endpoint content
//   L15 - OpenAPI operationId uniqueness across all models
//   L16 - min/max bounds appear in OAS schema components
//   L17 - ?include= when FK value is absent (nullable FK model)

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlcore"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/inproc"
	"github.com/xaleel/maniflex/middleware/auth"
	dbmw "github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/middleware/service"
	"github.com/xaleel/maniflex/middleware/validate"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Additional test models ────────────────────────────────────────────────────

// Article has fields needed for SlugifyField, TimestampWhen, and Tenancy tests.
type Article struct {
	maniflex.BaseModel
	Title       string  `json:"title"        db:"title"        mfx:"required,filterable,sortable"`
	Slug        *string `json:"slug"         db:"slug"         mfx:"filterable"`
	Body        string  `json:"body"         db:"body"         mfx:"required"`
	Status      string  `json:"status"       db:"status"       mfx:"required,filterable,enum:draft|published"`
	OrgID       *string `json:"org_id"       db:"org_id"       mfx:"filterable"`
	PublishedAt *string `json:"published_at" db:"published_at" mfx:"filterable"`
}

// Contact has phone and cross-field name pairs for RegexField and CrossFieldValidate.
type Contact struct {
	maniflex.BaseModel
	FirstName string  `json:"first_name" db:"first_name" mfx:"required"`
	LastName  string  `json:"last_name"  db:"last_name"  mfx:"required"`
	FullName  *string `json:"full_name"  db:"full_name"  mfx:"filterable"`
	Phone     *string `json:"phone"      db:"phone"      mfx:"filterable"`
	Email     string  `json:"email"      db:"email"      mfx:"required,filterable,unique"`
}

// Attachment has a nullable FK to User for L17 (optional FK include) tests.
type Attachment struct {
	maniflex.BaseModel
	Name   string `json:"name"    db:"name"    mfx:"required"`
	UserID string `json:"user_id" db:"user_id" mfx:"filterable,relation"`
}

// ── M6: response.Logging and response.Envelope ───────────────────────────────

func TestMedium_Logging(t *testing.T) {
	t.Parallel()

	t.Run("logging_emits_record_per_request", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Logging(slog.New(&captureSlogHandler{mu: &mu, records: &records})),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.MustID(srv.CreateUser("U", "log1@x.com", "viewer"))
		srv.GET("/users")

		mu.Lock()
		n := len(records)
		mu.Unlock()
		if n < 2 {
			t.Errorf("expected ≥2 log records (create + list), got %d", n)
		}
	})

	t.Run("logging_uses_warn_level_for_4xx", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					response.Logging(slog.New(&captureSlogHandler{mu: &mu, records: &records})),
					maniflex.AtPosition(maniflex.Before),
				)
			},
		})
		srv.GET("/users/00000000-0000-0000-0000-000000000000") // 404
		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()
		if len(recs) == 0 {
			t.Fatal("expected at least one log record")
		}
		if recs[0].Level != slog.LevelWarn {
			t.Errorf("4xx response must log at Warn, got %s", recs[0].Level)
		}
	})

	t.Run("logging_uses_info_level_for_2xx", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var records []slog.Record

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Logging(slog.New(&captureSlogHandler{mu: &mu, records: &records})),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.GET("/users")

		mu.Lock()
		recs := append([]slog.Record{}, records...)
		mu.Unlock()

		if len(recs) == 0 {
			t.Fatal("expected at least one log record")
		}
		if recs[0].Level != slog.LevelInfo {
			t.Errorf("2xx response must log at Info, got %s", recs[0].Level)
		}
	})
}

func TestMedium_Envelope(t *testing.T) {
	t.Parallel()

	t.Run("envelope_wraps_data_in_custom_structure", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Envelope(func(_ *maniflex.ServerContext, data any, _ *maniflex.ResponseMeta) any {
						return map[string]any{"result": data, "ok": true}
					}),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.CreateUser("U", "env1@x.com", "viewer").AssertStatus(http.StatusCreated)
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			// APIResponse.Write wraps as {"data": ctx.Response.Data}
			// After Envelope, ctx.Response.Data is {"result":…, "ok":true}
			dataVal, ok := body["data"].(map[string]any)
			if !ok {
				t.Fatalf("outer 'data' must be an object after Envelope, got %T", body["data"])
			}
			if _, hasResult := dataVal["result"]; !hasResult {
				t.Error("wrapped object must contain 'result' key")
			}
			if ok2, _ := dataVal["ok"].(bool); !ok2 {
				t.Error("wrapped object must have ok=true")
			}
		})
	})

	t.Run("envelope_receives_meta_on_list", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var capturedMeta *maniflex.ResponseMeta

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Envelope(func(_ *maniflex.ServerContext, data any, meta *maniflex.ResponseMeta) any {
						mu.Lock()
						capturedMeta = meta
						mu.Unlock()
						return map[string]any{"items": data, "pagination": meta}
					}),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.CreateUser("U", "env2@x.com", "viewer").AssertStatus(http.StatusCreated)
		srv.GET("/users").AssertStatus(http.StatusOK)

		mu.Lock()
		m := capturedMeta
		mu.Unlock()
		if m == nil {
			t.Error("Envelope must receive non-nil meta for list responses")
		}
	})

	t.Run("envelope_skips_error_responses", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Envelope(func(_ *maniflex.ServerContext, data any, _ *maniflex.ResponseMeta) any {
						return map[string]any{"wrapped": data}
					}),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		resp := srv.GET("/users/00000000-0000-0000-0000-000000000000")
		resp.AssertStatus(http.StatusNotFound)
		resp.AssertJSON(func(body map[string]any) {
			if _, ok := body["error"]; !ok {
				t.Error("error response must keep standard {error:…} shape, not be wrapped")
			}
		})
	})

	t.Run("envelope_meta_nil_prevents_duplicate_meta_block", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.Envelope(func(_ *maniflex.ServerContext, data any, _ *maniflex.ResponseMeta) any {
						return map[string]any{"payload": data}
					}),
					maniflex.AtPosition(maniflex.After),
				)
			},
		})
		res := srv.CreateUser("U", "env3@x.com", "viewer")
		id := res.Data()["payload"].(map[string]any)["id"].(string)
		resp := srv.GET("/users/" + id)
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			if _, hasMeta := body["meta"]; hasMeta {
				t.Error("separate meta block must not appear after Envelope on a single record")
			}
		})
	})
}

// ── M7: service field transforms ──────────────────────────────────────────────

func TestMedium_ServiceFieldTransforms(t *testing.T) {
	t.Parallel()

	// SlugifyField
	t.Run("slugify_derives_slug_from_title", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.SlugifyField("title", "slug"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		resp := srv.POST("/articles", map[string]any{"title": "Hello World!", "body": "B", "status": "draft"})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "slug derived", testutil.Field(t, resp.Data(), "slug"), "hello-world")
	})

	t.Run("slugify_does_not_overwrite_explicit_slug", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.SlugifyField("title", "slug"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/articles", map[string]any{
			"title": "Hello", "slug": "custom-slug", "body": "B", "status": "draft",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "explicit slug kept", testutil.Field(t, resp.Data(), "slug"), "custom-slug")
	})

	t.Run("slugify_produces_lowercase_alphanumeric_hyphens_only", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.SlugifyField("title", "slug"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/articles", map[string]any{
			"title": "Spaces & Symbols! 123", "body": "B", "status": "draft",
		})
		resp.AssertStatus(http.StatusCreated)
		slug := testutil.Field(t, resp.Data(), "slug")
		for _, ch := range slug {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-') {
				t.Errorf("slug contains invalid char %q: %q", ch, slug)
			}
		}
		testutil.AssertNotEmpty(t, "slug not empty", slug)
	})

	// StripField
	t.Run("strip_field_removed_before_db_write", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var bodyAtDB map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.StripField("phone"),
					maniflex.ForModel("Contact"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					bodyAtDB = ctx.ParsedBody.Map()
					mu.Unlock()
					return next()
				}, maniflex.ForModel("Contact"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "strip@x.com", "phone": "123",
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		body := bodyAtDB
		mu.Unlock()
		if _, ok := body["phone"]; ok {
			t.Error("StripField must remove 'phone' from DB write data")
		}
	})

	t.Run("strip_multiple_fields_simultaneously", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var bodyAtDB map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.StripField("phone", "full_name"),
					maniflex.ForModel("Contact"))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpCreate {
						mu.Lock()
						bodyAtDB = ctx.ParsedBody.Map()
						mu.Unlock()
					}
					return next()
				}, maniflex.ForModel("Contact"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "smf@x.com",
			"phone": "123", "full_name": "A B",
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		body := bodyAtDB
		mu.Unlock()
		for _, field := range []string{"phone", "full_name"} {
			if _, ok := body[field]; ok {
				t.Errorf("StripField must remove %q from DB write data", field)
			}
		}
	})

	// CopyField
	t.Run("copy_field_sets_dest_from_source_when_dest_absent", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.CopyField("email", "full_name"),
					maniflex.ForModel("Contact"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "copy@x.com",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "full_name copied from email",
			testutil.Field(t, resp.Data(), "full_name"), "copy@x.com")
	})

	t.Run("copy_field_does_not_overwrite_existing_dest", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.CopyField("email", "full_name"),
					maniflex.ForModel("Contact"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "copy2@x.com", "full_name": "Manual",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "explicit full_name preserved",
			testutil.Field(t, resp.Data(), "full_name"), "Manual")
	})

	// TimestampWhen
	t.Run("timestamp_when_sets_field_on_condition_match", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.TimestampWhen("published_at", "status", "published"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
			},
		})
		resp := srv.POST("/articles", map[string]any{
			"title": "T", "body": "B", "status": "published",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertNotEmpty(t, "published_at set", testutil.Field(t, resp.Data(), "published_at"))
	})

	t.Run("timestamp_when_skips_field_on_condition_mismatch", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.TimestampWhen("published_at", "status", "published"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/articles", map[string]any{
			"title": "T", "body": "B", "status": "draft",
		})
		resp.AssertStatus(http.StatusCreated)
		if v := resp.Data()["published_at"]; v != nil && v != "" {
			t.Errorf("published_at must not be set when status=draft, got: %v", v)
		}
	})

	// OwnerScope
	t.Run("owner_scope_injects_auth_user_id_into_body", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var captured map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Auth = &maniflex.AuthInfo{UserID: "owner-abc"}
					return next()
				})
				s.Pipeline.Service.Register(service.OwnerScope("org_id"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					captured = ctx.ParsedBody.Map()
					mu.Unlock()
					return next()
				}, maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/articles", map[string]any{"title": "T", "body": "B", "status": "draft"}).
			AssertStatus(http.StatusCreated)

		mu.Lock()
		body := captured
		mu.Unlock()
		testutil.AssertEqual(t, "org_id injected", fmt.Sprintf("%v", body["org_id"]), "owner-abc")
	})

	t.Run("owner_scope_skips_when_no_auth", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var captured map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(service.OwnerScope("org_id"),
					maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					captured = ctx.ParsedBody.Map()
					mu.Unlock()
					return next()
				}, maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/articles", map[string]any{"title": "T", "body": "B", "status": "draft"}).
			AssertStatus(http.StatusCreated)

		mu.Lock()
		body := captured
		mu.Unlock()
		// With no auth, org_id must not be set by OwnerScope
		if v, ok := body["org_id"]; ok && v != "" && v != nil {
			t.Errorf("OwnerScope must not inject org_id when auth is nil, got: %v", v)
		}
	})

	// Emit
	t.Run("emit_publishes_event_after_successful_create", func(t *testing.T) {
		t.Parallel()
		received := make(chan events.Event, 1)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					events.Emit(&chanEventBus{ch: received}),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.MustID(srv.CreateUser("U", "emit1@x.com", "viewer"))

		select {
		case evt := <-received:
			testutil.AssertEqual(t, "event model", evt.Model, "User")
			testutil.AssertEqual(t, "event op", string(evt.Operation), "create")
			if evt.Time.IsZero() {
				t.Error("Time must be set on emitted events")
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("Emit: event not received within 500ms")
		}
	})

	t.Run("emit_default_event_type_is_model_dot_operation", func(t *testing.T) {
		t.Parallel()
		received := make(chan events.Event, 2)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					events.Emit(&chanEventBus{ch: received}),
					maniflex.ForOperation(maniflex.OpCreate, maniflex.OpDelete), maniflex.AtPosition(maniflex.After))
			},
		})
		id := srv.MustID(srv.CreateUser("U", "emit2@x.com", "viewer"))
		srv.DELETE("/users/" + id)

		evts := drainExtChan(received, 2, 500*time.Millisecond)
		types := make([]string, len(evts))
		for i, e := range evts {
			types[i] = e.Type
		}
		testutil.AssertContains(t, "created event", types, "user.created")
		testutil.AssertContains(t, "deleted event", types, "user.deleted")
	})

	t.Run("emit_does_not_fire_on_failed_request", func(t *testing.T) {
		t.Parallel()
		var count int64

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					events.Emit(&countingEventBus{n: &count}),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.POST("/users", map[string]any{}).AssertStatus(http.StatusUnprocessableEntity)
		time.Sleep(100 * time.Millisecond)
		if atomic.LoadInt64(&count) != 0 {
			t.Error("Emit must not fire when request fails before DB")
		}
	})

	// Webhook (now a subscriber on an inproc bus rather than a pipeline middleware)
	t.Run("webhook_posts_json_payload_to_target", func(t *testing.T) {
		t.Parallel()
		received := make(chan []byte, 1)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- body
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(target.Close)

		bus := inproc.New()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		bus.Subscribe(ctx, events.Subscription{ //nolint:errcheck
			Patterns: []string{"*"},
			Handler:  events.Webhook(events.WebhookConfig{URL: target.URL, Timeout: 2 * time.Second}),
		})

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					events.Emit(bus),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.MustID(srv.CreateUser("U", "wh@x.com", "viewer"))

		select {
		case body := <-received:
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("webhook body invalid JSON: %v body: %s", err, body)
			}
			testutil.AssertEqual(t, "webhook model", payload["model"], "User")
			testutil.AssertEqual(t, "webhook op", payload["operation"], "create")
		case <-time.After(2 * time.Second):
			t.Error("webhook payload not received within 2s")
		}
	})

	t.Run("webhook_signs_with_hmac_when_secret_set", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var sig string
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sig = r.Header.Get("X-Webhook-Signature")
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(target.Close)

		bus := inproc.New()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		bus.Subscribe(ctx, events.Subscription{ //nolint:errcheck
			Patterns: []string{"*"},
			Handler:  events.Webhook(events.WebhookConfig{URL: target.URL, Secret: "whsec_test"}),
		})

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					events.Emit(bus),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.MustID(srv.CreateUser("U", "whs@x.com", "viewer"))
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		header := sig
		mu.Unlock()
		if !strings.HasPrefix(header, "sha256=") {
			t.Errorf("X-Webhook-Signature must start with sha256=, got: %q", header)
		}
	})
}

// ── M8: validate.UniqueField self-exclusion on update ─────────────────────────

func TestMedium_UniqueFieldSelfExclusion(t *testing.T) {
	t.Parallel()

	// We need the raw *sql.DB to pass to validate.UniqueField.
	// The helper newRawServer opens a shared *sql.DB, wires sqlcore, and
	// returns both the httptest.Server URL and the raw *sql.DB.
	newRawServer := func(t *testing.T, models ...any) (string, *sql.DB) {
		t.Helper()
		rawDB, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		rawDB.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;")
		t.Cleanup(func() { rawDB.Close() })

		server := maniflex.New(maniflex.Config{PathPrefix: "/api", DisableAutoMigrate: true})
		for _, m := range models {
			server.MustRegister(m)
		}
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
		return ts.URL, rawDB
	}

	t.Run("create_duplicate_email_returns_non_201", func(t *testing.T) {
		t.Parallel()
		base, _ := newRawServer(t, testutil.User{})

		if s := postJSON(t, base+"/api/users", map[string]any{
			"name": "A", "email": "uniq@x.com", "password": "s",
		}); s != http.StatusCreated {
			t.Fatalf("first create: got %d", s)
		}
		s := postJSON(t, base+"/api/users", map[string]any{
			"name": "B", "email": "uniq@x.com", "password": "s",
		})
		if s == http.StatusCreated {
			t.Error("duplicate email must be rejected with 422")
		}
		if s != http.StatusUnprocessableEntity {
			t.Errorf("expected 422, got %d", s)
		}
	})

	t.Run("update_without_changing_unique_field_passes", func(t *testing.T) {
		// Email is immutable on User so it's stripped on PATCH anyway.
		// Use a fresh User model via raw server; patch only the name field.
		t.Parallel()
		base, _ := newRawServer(t, testutil.User{})

		s1 := postJSON(t, base+"/api/users", map[string]any{
			"name": "Alice", "email": "self@x.com", "password": "s",
		})
		if s1 != http.StatusCreated {
			t.Fatalf("create: got %d", s1)
		}
		// Read the ID back
		resp, _ := http.Get(base + "/api/users")
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		items, _ := body["data"].([]any)
		if len(items) == 0 {
			t.Fatal("no items returned")
		}
		id := items[0].(map[string]any)["id"].(string)

		// PATCH without email — must not trigger uniqueness check (email is immutable, stripped)
		req, _ := http.NewRequest(http.MethodPatch, base+"/api/users/"+id,
			strings.NewReader(`{"name":"Alicia"}`))
		req.Header.Set("Content-Type", "application/json")
		res, _ := http.DefaultClient.Do(req)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("patch without email change: got %d, want 200", res.StatusCode)
		}
	})
}

// ── M9: validate.CrossFieldValidate and validate.RegexField ───────────────────

func TestMedium_CrossFieldAndRegex(t *testing.T) {
	t.Parallel()

	t.Run("cross_field_passes_when_fn_returns_nil", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(validate.CrossFieldValidate(
					func(body map[string]any) error {
						fn, _ := body["first_name"].(string)
						ln, _ := body["last_name"].(string)
						if fn == ln {
							return fmt.Errorf("first and last name must differ")
						}
						return nil
					}), maniflex.ForModel("Contact"))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "Alice", "last_name": "Smith", "email": "cf1@x.com",
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("cross_field_returns_422_on_fn_error", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(validate.CrossFieldValidate(
					func(body map[string]any) error {
						fn, _ := body["first_name"].(string)
						ln, _ := body["last_name"].(string)
						if fn == ln {
							return fmt.Errorf("first and last name must differ")
						}
						return nil
					}), maniflex.ForModel("Contact"))
			},
		})
		resp := srv.POST("/contacts", map[string]any{
			"first_name": "Same", "last_name": "Same", "email": "cf2@x.com",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		testutil.AssertEqual(t, "error code", resp.ErrorCode(), "VALIDATION_ERROR")
	})

	t.Run("cross_field_error_message_propagated", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(validate.CrossFieldValidate(
					func(_ map[string]any) error { return fmt.Errorf("custom cross-field error") }),
					maniflex.ForModel("Contact"))
			},
		})
		resp := srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "cf3@x.com",
		})
		resp.AssertStatus(http.StatusUnprocessableEntity)
		resp.AssertJSON(func(body map[string]any) {
			msg := body["error"].(map[string]any)["message"].(string)
			if !strings.Contains(msg, "custom cross-field error") {
				t.Errorf("error message must contain custom text, got: %q", msg)
			}
		})
	})

	t.Run("regex_field_passes_matching_value", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RegexField("phone", `^\+?[0-9\s\-]{7,15}$`),
					maniflex.ForModel("Contact"))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "rx1@x.com", "phone": "+1 555-1234",
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("regex_field_returns_422_on_non_match", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RegexField("phone", `^\\+?[0-9\\s\\-]{7,15}$`),
					maniflex.ForModel("Contact"))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "rx2@x.com", "phone": "not-a-phone!",
		}).AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("regex_field_absent_value_skips_validation", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Contact{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RegexField("phone", `^\\+?[0-9\\s\\-]{7,15}$`),
					maniflex.ForModel("Contact"))
			},
		})
		srv.POST("/contacts", map[string]any{
			"first_name": "A", "last_name": "B", "email": "rx3@x.com",
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("regex_field_non_string_type_skips_validation", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RegexField("score", `^[0-9]+$`),
					maniflex.ForModel("User"))
			},
		})
		// score is an int — must not trigger regex on non-string
		srv.POST("/users", map[string]any{
			"name": "A", "email": "rx4@x.com", "password": "s", "score": 42,
		}).AssertStatus(http.StatusCreated)
	})
}

// fakeRateLimitBackend is a deterministic in-memory dbmw.RateLimitBackend
// used to verify that RateLimit delegates to a custom Backend.
type fakeRateLimitBackend struct {
	mu     sync.Mutex
	counts map[string]int64
	calls  int
	err    error
}

func (f *fakeRateLimitBackend) Increment(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	f.counts[key]++
	return f.counts[key], nil
}

// ── M10: db.RateLimit, db.Paginate, db.Tenancy, db.Invalidate ────────────────

func TestMedium_DBMiddleware(t *testing.T) {
	t.Parallel()

	// RateLimit
	t.Run("rate_limit_allows_under_threshold", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 5,
						KeyFunc:           func(*maniflex.ServerContext) string { return "test-key" },
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		for range 5 {
			srv.GET("/users").AssertStatus(http.StatusOK)
		}
	})

	t.Run("rate_limit_returns_429_when_exceeded", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 3,
						KeyFunc:           func(*maniflex.ServerContext) string { return "fixed" },
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		for range 3 {
			srv.GET("/users").AssertStatus(http.StatusOK)
		}
		srv.GET("/users").AssertStatus(http.StatusTooManyRequests)
	})

	t.Run("rate_limit_sets_retry_after_on_429", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 1,
						KeyFunc:           func(*maniflex.ServerContext) string { return "hdr" },
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.GET("/users")
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusTooManyRequests)
		if resp.Header.Get("Retry-After") == "" {
			t.Error("429 response must include Retry-After header")
		}
	})

	t.Run("rate_limit_different_keys_never_collide", func(t *testing.T) {
		t.Parallel()
		var seq int64
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 2,
						KeyFunc: func(*maniflex.ServerContext) string {
							return fmt.Sprintf("key-%d", atomic.AddInt64(&seq, 1))
						},
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		for range 10 {
			srv.GET("/users").AssertStatus(http.StatusOK)
		}
	})

	t.Run("rate_limit_uses_custom_backend_when_provided", func(t *testing.T) {
		t.Parallel()
		backend := &fakeRateLimitBackend{counts: map[string]int64{}}
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 2,
						KeyFunc:           func(*maniflex.ServerContext) string { return "shared" },
						Backend:           backend,
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusTooManyRequests)
		if backend.calls < 3 {
			t.Errorf("backend Increment should have been called at least 3 times, got %d", backend.calls)
		}
	})

	t.Run("rate_limit_propagates_backend_error", func(t *testing.T) {
		t.Parallel()
		backend := &fakeRateLimitBackend{err: errors.New("boom")}
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.RateLimit(dbmw.RateLimitConfig{
						RequestsPerMinute: 5,
						KeyFunc:           func(*maniflex.ServerContext) string { return "err" },
						Backend:           backend,
					}), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.GET("/users").AssertStatus(http.StatusInternalServerError)
	})

	// Paginate
	t.Run("paginate_clamps_limit_above_max", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(dbmw.Paginate(5), maniflex.ForModel("User"))
			},
		})
		for i := range 10 {
			srv.MustID(srv.CreateUser(fmt.Sprintf("U%d", i), fmt.Sprintf("pg%d@x.com", i), "viewer"))
		}
		items := srv.GET("/users?limit=100").DataList()
		if len(items) > 5 {
			t.Errorf("Paginate(5) must cap at 5, got %d", len(items))
		}
		testutil.AssertEqual(t, "meta.limit clamped", srv.GET("/users?limit=100").Meta()["limit"], float64(5))
	})

	t.Run("paginate_does_not_raise_a_lower_limit", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(dbmw.Paginate(50), maniflex.ForModel("User"))
			},
		})
		srv.MustID(srv.CreateUser("U", "pgn@x.com", "viewer"))
		testutil.AssertEqual(t, "limit 3 unchanged", srv.GET("/users?limit=3").Meta()["limit"], float64(3))
	})

	// Tenancy
	t.Run("tenancy_injects_tenant_id_into_create_body", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var captured map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Tenancy("org_id", func(*maniflex.ServerContext) string { return "org-xyz" }),
					maniflex.ForModel("Article"))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpCreate {
						mu.Lock()
						captured = ctx.ParsedBody.Map()
						mu.Unlock()
					}
					return next()
				}, maniflex.ForModel("Article"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/articles", map[string]any{"title": "T", "body": "B", "status": "draft"}).
			AssertStatus(http.StatusCreated)

		mu.Lock()
		body := captured
		mu.Unlock()
		testutil.AssertEqual(t, "org_id injected", fmt.Sprintf("%v", body["org_id"]), "org-xyz")
	})

	t.Run("tenancy_enforces_filter_so_other_tenant_data_invisible", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Tenancy("org_id", func(ctx *maniflex.ServerContext) string {
						if org := ctx.Request.Header.Get("X-Org"); org != "" {
							return org
						}
						return "default"
					}), maniflex.ForModel("Article"))
			},
		})
		// Create one article (tenancy injects org_id="default")
		srv.POST("/articles", map[string]any{"title": "A", "body": "B", "status": "draft"}).
			AssertStatus(http.StatusCreated)

		items := srv.GET("/articles", map[string]string{"X-Org": "default"}).DataList()
		testutil.AssertLen(t, "same tenant sees article", items, 1)

		items2 := srv.GET("/articles", map[string]string{"X-Org": "other-org"}).DataList()
		testutil.AssertLen(t, "different tenant sees nothing", items2, 0)
	})

	t.Run("tenancy_returns_403_when_resolver_returns_empty", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Article{}},
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Tenancy("org_id", func(*maniflex.ServerContext) string { return "" }),
					maniflex.ForModel("Article"))
			},
		})
		srv.GET("/articles").AssertStatus(http.StatusForbidden)
	})

	// Invalidate
	t.Run("invalidate_deletes_keys_after_successful_write", func(t *testing.T) {
		t.Parallel()
		deleted := make(chan string, 10)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Invalidate(&chanCache2{ch: deleted}, func(ctx *maniflex.ServerContext) []string {
						return []string{"users:list", "users:" + ctx.ResourceID}
					}),
					maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
					maniflex.AtPosition(maniflex.After))
			},
		})
		srv.MustID(srv.CreateUser("U", "inv@x.com", "viewer"))
		time.Sleep(200 * time.Millisecond)

		var keys []string
	loop:
		for {
			select {
			case k := <-deleted:
				keys = append(keys, k)
			default:
				break loop
			}
		}
		testutil.AssertContains(t, "users:list invalidated", keys, "users:list")
	})

	t.Run("invalidate_does_not_fire_on_failed_request", func(t *testing.T) {
		t.Parallel()
		var count int64

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.Invalidate(&countingCache2{n: &count}, func(*maniflex.ServerContext) []string { return []string{"k"} }),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After))
			},
		})
		srv.POST("/users", map[string]any{}).AssertStatus(http.StatusUnprocessableEntity)
		time.Sleep(100 * time.Millisecond)
		if atomic.LoadInt64(&count) != 0 {
			t.Error("Invalidate must not fire when request fails")
		}
	})
}

// ── M10b: db.CacheQuery (query result cache) ─────────────────────────────────

func TestMedium_CacheQuery(t *testing.T) {
	t.Parallel()

	listKey := func(*maniflex.ServerContext) string { return "users:list" }

	t.Run("hit_serves_stale_result_and_skips_db", func(t *testing.T) {
		t.Parallel()
		cache := maniflex.NewMemoryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.CacheQuery(cache, dbmw.CacheConfig{TTL: time.Minute, KeyFunc: listKey}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.MustID(srv.CreateUser("A", "a@cache.com", "viewer"))
		if got := len(srv.GET("/users").DataList()); got != 1 {
			t.Fatalf("first list: got %d users, want 1", got)
		}
		// A second user lands in the DB but the cached list is served unchanged.
		srv.MustID(srv.CreateUser("B", "b@cache.com", "viewer"))
		if got := len(srv.GET("/users").DataList()); got != 1 {
			t.Errorf("cached list: got %d users, want 1 (DB read should be skipped)", got)
		}
	})

	t.Run("miss_then_hit_stores_once_and_reuses", func(t *testing.T) {
		t.Parallel()
		cache := newCountingQueryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.CacheQuery(cache, dbmw.CacheConfig{TTL: time.Minute, KeyFunc: listKey}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.MustID(srv.CreateUser("A", "a@reuse.com", "viewer"))
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
		if got := cache.SetCount(); got != 1 {
			t.Errorf("Set called %d times, want 1 (miss stores, hit reuses)", got)
		}
		if got := cache.HitCount(); got < 1 {
			t.Errorf("hits = %d, want >= 1 (second list should be served from cache)", got)
		}
	})

	t.Run("invalidate_pair_refreshes_after_write", func(t *testing.T) {
		t.Parallel()
		cache := maniflex.NewMemoryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.CacheQuery(cache, dbmw.CacheConfig{TTL: time.Minute, KeyFunc: listKey}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
				s.Pipeline.DB.Register(
					dbmw.Invalidate(cache, func(*maniflex.ServerContext) []string { return []string{"users:list"} }),
					maniflex.ForModel("User"),
					maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
					maniflex.AtPosition(maniflex.After))
			},
		})
		srv.MustID(srv.CreateUser("A", "a@inv.com", "viewer"))
		if got := len(srv.GET("/users").DataList()); got != 1 {
			t.Fatalf("first list: got %d users, want 1", got)
		}
		srv.MustID(srv.CreateUser("B", "b@inv.com", "viewer")) // After-write Invalidate evicts the key
		time.Sleep(200 * time.Millisecond)                     // Invalidate deletes in the background
		if got := len(srv.GET("/users").DataList()); got != 2 {
			t.Errorf("after invalidation: got %d users, want 2 (cache should be refreshed)", got)
		}
	})

	t.Run("empty_key_disables_caching", func(t *testing.T) {
		t.Parallel()
		cache := newCountingQueryCache()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.CacheQuery(cache, dbmw.CacheConfig{
						TTL:     time.Minute,
						KeyFunc: func(*maniflex.ServerContext) string { return "" },
					}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.MustID(srv.CreateUser("A", "a@nokey.com", "viewer"))
		srv.GET("/users").AssertStatus(http.StatusOK)
		srv.GET("/users").AssertStatus(http.StatusOK)
		if g, sset := cache.GetCount(), cache.SetCount(); g != 0 || sset != 0 {
			t.Errorf("empty key must not touch the cache: gets=%d sets=%d", g, sset)
		}
	})

	t.Run("unexpected_cached_list_type_is_a_miss", func(t *testing.T) {
		t.Parallel()
		// A store that hands back a bare map for a list (as a naive JSON-decoding
		// Redis adapter would) must yield a miss, not a panic in the Response step.
		cache := &badListCache{}
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					dbmw.CacheQuery(cache, dbmw.CacheConfig{TTL: time.Minute, KeyFunc: listKey}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList))
			},
		})
		srv.MustID(srv.CreateUser("A", "a@bad.com", "viewer"))
		resp := srv.GET("/users")
		resp.AssertStatus(http.StatusOK)
		if got := len(resp.DataList()); got != 1 {
			t.Errorf("got %d users, want 1 (fresh DB read on unusable cache value)", got)
		}
	})
}

// ── M11: auth.RequireOwner and auth.AllowPublicRead ───────────────────────────

func TestMedium_RequireOwnerAndPublicRead(t *testing.T) {
	t.Parallel()

	t.Run("require_owner_injects_user_id_on_create", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var captured map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Auth = &maniflex.AuthInfo{UserID: "user-owner-123"}
					return next()
				})
				s.Pipeline.Auth.Register(auth.RequireOwner("owner"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpCreate {
						mu.Lock()
						captured = ctx.ParsedBody.Map()
						mu.Unlock()
					}
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users", map[string]any{"email": "ro@x.com", "password": "s", "name": "user_name"}).
			AssertStatus(http.StatusCreated)

		mu.Lock()
		body := captured
		mu.Unlock()
		testutil.AssertEqual(t, "name injected from auth.UserID",
			fmt.Sprintf("%v", body["owner"]), "user-owner-123")
	})

	t.Run("require_owner_without_auth_returns_401", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.RequireOwner("user_id"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "U", "email": "rona@x.com", "password": "s",
		}).AssertStatus(http.StatusUnauthorized)
	})

	t.Run("require_owner_admin_role_bypasses_injection", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var captured map[string]any

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Auth = &maniflex.AuthInfo{UserID: "admin-999", Roles: []string{"admin"}}
					return next()
				})
				// "admin" role bypasses RequireOwner injection
				s.Pipeline.Auth.Register(auth.RequireOwner("name", "admin"), maniflex.ForOperation(maniflex.OpCreate))
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpCreate {
						mu.Lock()
						captured = ctx.ParsedBody.Map()
						mu.Unlock()
					}
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "Custom Name", "email": "bypass@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)

		mu.Lock()
		body := captured
		mu.Unlock()
		testutil.AssertEqual(t, "admin name not overwritten",
			fmt.Sprintf("%v", body["name"]), "Custom Name")
	})

	t.Run("allow_public_read_passes_list_without_auth", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) { s.Pipeline.Auth.Register(auth.AllowPublicRead()) },
		})
		srv.GET("/users").AssertStatus(http.StatusOK)
	})

	t.Run("allow_public_read_passes_single_read_without_auth", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// needed for srv.CreateUser
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Auth = &maniflex.AuthInfo{UserID: "u1"}
					return next()
				})
				s.Pipeline.Auth.Register(auth.AllowPublicRead())
			},
		})
		id := srv.MustID(srv.CreateUser("U", "apr@x.com", "viewer"))
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
	})

	t.Run("allow_public_read_blocks_create_without_auth", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) { s.Pipeline.Auth.Register(auth.AllowPublicRead()) },
		})
		srv.POST("/users", map[string]any{
			"name": "U", "email": "apc@x.com", "password": "s",
		}).AssertStatus(http.StatusUnauthorized)
	})

	t.Run("allow_public_read_allows_write_when_auth_already_set", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Auth = &maniflex.AuthInfo{UserID: "u1"}
					return next()
				})
				s.Pipeline.Auth.Register(auth.AllowPublicRead())
			},
		})
		srv.POST("/users", map[string]any{
			"name": "U", "email": "apw@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)
	})
}

// ── L12: Custom ModelConfig.TableName ─────────────────────────────────────────

func TestLower_CustomTableName(t *testing.T) {
	t.Parallel()

	t.Run("model_served_under_custom_table_name", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Post{}, maniflex.ModelConfig{TableName: "entries"}},
		})
		srv.GET("/entries").AssertStatus(http.StatusOK)
	})

	t.Run("original_plural_name_not_routed", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Post{}, maniflex.ModelConfig{TableName: "entries"}},
		})
		if resp := srv.GET("/posts"); resp.Status == http.StatusOK {
			t.Error("/posts must not be routed when TableName is overridden to 'entries'")
		}
	})

	t.Run("crud_works_on_custom_table_name", func(t *testing.T) {
		t.Parallel()
		type Member struct {
			maniflex.BaseModel
			Name string `json:"name" db:"name" mfx:"required,filterable"`
		}
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{Member{}, maniflex.ModelConfig{TableName: "members"}},
		})
		resp := srv.POST("/members", map[string]any{"name": "Alice"})
		resp.AssertStatus(http.StatusCreated)
		id := resp.ID()
		srv.GET("/members/" + id).AssertStatus(http.StatusOK)
		srv.PATCH("/members/"+id, map[string]any{"name": "Alicia"}).AssertStatus(http.StatusOK)
		srv.DELETE("/members/" + id).AssertStatus(http.StatusNoContent)
	})

	t.Run("openapi_uses_custom_table_name_in_paths", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.Post{}, maniflex.ModelConfig{TableName: "entries"}},
		})
		srv.GET("/openapi.json").AssertJSON(func(body map[string]any) {
			paths, _ := body["paths"].(map[string]any)
			if _, ok := paths["/entries"]; !ok {
				t.Error("custom table name /entries must appear in OpenAPI paths")
			}
		})
	})
}

// ── L13: PathPrefix customisation ─────────────────────────────────────────────

func TestLower_PathPrefix(t *testing.T) {
	t.Parallel()

	t.Run("v1_prefix_routes_work", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{PathPrefix: "/v1"})
		srv.Do("GET", srv.Server.URL+"/v1/users", nil).AssertStatus(http.StatusOK)
	})

	t.Run("default_api_prefix_absent_with_v1_config", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{PathPrefix: "/v1"})
		if resp := srv.Do("GET", srv.Server.URL+"/api/users", nil); resp.Status == http.StatusOK {
			t.Error("/api/users must not be served when PathPrefix is /v1")
		}
	})

	t.Run("crud_works_on_custom_prefix", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{PathPrefix: "/v2"})
		srv.Do("POST", srv.Server.URL+"/v2/users", map[string]any{
			"name": "A", "email": "pfx@x.com", "password": "s",
		}).AssertStatus(http.StatusCreated)
	})

	t.Run("health_available_under_custom_prefix", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{PathPrefix: "/myapi"})
		srv.Do("GET", srv.Server.URL+"/myapi/health", nil).AssertStatus(http.StatusOK)
	})

	t.Run("openapi_spec_available_under_custom_prefix", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{PathPrefix: "/v3"})
		srv.Do("GET", srv.Server.URL+"/v3/openapi.json", nil).AssertStatus(http.StatusOK)
	})
}

// ── L14: GET /health endpoint ──────────────────────────────────────────────────

func TestLower_HealthEndpoint(t *testing.T) {
	t.Parallel()

	t.Run("health_returns_200", func(t *testing.T) {
		t.Parallel()
		testutil.NewServer(t, testutil.Options{}).GET("/health").AssertStatus(http.StatusOK)
	})

	t.Run("health_returns_status_ok_json", func(t *testing.T) {
		t.Parallel()
		testutil.NewServer(t, testutil.Options{}).GET("/health").AssertJSON(func(body map[string]any) {
			if body["status"] != "ok" {
				t.Errorf("health must return {status:ok}, got: %v", body)
			}
		})
	})

	t.Run("health_content_type_is_json", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/health")
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("health must return application/json, got: %q", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("health_bypasses_model_pipeline_auth_middleware", func(t *testing.T) {
		// Model auth middleware must not affect the plain /health chi handler.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					ctx.Abort(http.StatusUnauthorized, "UNAUTHORIZED", "no access")
					return nil
				})
			},
		})
		srv.GET("/health").AssertStatus(http.StatusOK)
	})
}

// ── L15: OpenAPI operationId uniqueness ───────────────────────────────────────

func TestLower_OpenAPIOperationIDs(t *testing.T) {
	t.Parallel()

	t.Run("all_operation_ids_unique_across_all_models", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/openapi.json")
		resp.AssertStatus(http.StatusOK)
		resp.AssertJSON(func(body map[string]any) {
			paths, _ := body["paths"].(map[string]any)
			seen := make(map[string]string)
			for path, item := range paths {
				for _, method := range []string{"get", "post", "patch", "delete"} {
					op, ok := item.(map[string]any)[method].(map[string]any)
					if !ok {
						continue
					}
					id, _ := op["operationId"].(string)
					if id == "" {
						t.Errorf("empty operationId at %s %s", method, path)
						continue
					}
					if prev, dup := seen[id]; dup {
						t.Errorf("duplicate operationId %q: %s vs %s %s", id, prev, method, path)
					}
					seen[id] = method + " " + path
				}
			}
		})
	})

	t.Run("operation_ids_contain_model_name", func(t *testing.T) {
		t.Parallel()
		resp := testutil.NewServer(t, testutil.Options{}).GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			listOp := paths["/users"].(map[string]any)["get"].(map[string]any)
			if id, _ := listOp["operationId"].(string); !strings.Contains(id, "User") {
				t.Errorf("list operationId must contain 'User', got: %q", id)
			}
		})
	})

	t.Run("single_model_produces_exactly_five_operation_ids", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{Models: []any{testutil.User{}}})
		resp := srv.GET("/openapi.json")
		resp.AssertJSON(func(body map[string]any) {
			paths := body["paths"].(map[string]any)
			var ids []string
			for _, item := range paths {
				for _, method := range []string{"get", "post", "patch", "delete"} {
					if op, ok := item.(map[string]any)[method].(map[string]any); ok {
						if id, _ := op["operationId"].(string); id != "" {
							ids = append(ids, id)
						}
					}
				}
			}
			if len(ids) != 5 {
				t.Errorf("single model must produce 5 operationIds, got %d: %v", len(ids), ids)
			}
		})
	})
}

// ── L16: min/max bounds appear in OAS schema ──────────────────────────────────

func TestLower_OpenAPIMinMaxSchema(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{Models: []any{RatedItem{}}})

	t.Run("minimum_appears_in_create_schema", func(t *testing.T) {
		t.Parallel()
		srv.GET("/openapi.json").AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			create, _ := schemas["RatedItemCreate"].(map[string]any)
			props, _ := create["properties"].(map[string]any)
			score, _ := props["score"].(map[string]any)
			if score["minimum"] == nil {
				t.Error("score must have 'minimum' in RatedItemCreate schema")
			}
			if score["maximum"] == nil {
				t.Error("score must have 'maximum' in RatedItemCreate schema")
			}
		})
	})

	t.Run("minimum_appears_in_response_schema", func(t *testing.T) {
		t.Parallel()
		srv.GET("/openapi.json").AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			schema, _ := schemas["RatedItem"].(map[string]any)
			props, _ := schema["properties"].(map[string]any)
			score, _ := props["score"].(map[string]any)
			if score["minimum"] == nil {
				t.Error("score must have 'minimum' in RatedItem response schema")
			}
		})
	})

	t.Run("min_max_values_are_correct_in_spec", func(t *testing.T) {
		t.Parallel()
		srv.GET("/openapi.json").AssertJSON(func(body map[string]any) {
			schemas := body["components"].(map[string]any)["schemas"].(map[string]any)
			create := schemas["RatedItemCreate"].(map[string]any)
			props := create["properties"].(map[string]any)
			score := props["score"].(map[string]any)
			testutil.AssertEqual(t, "minimum is 1", score["minimum"], float64(1))
			testutil.AssertEqual(t, "maximum is 10", score["maximum"], float64(10))
		})
	})
}

// ── L17: ?include= when FK value is absent (nullable FK) ──────────────────────

func TestLower_IncludeNullableFK(t *testing.T) {
	t.Parallel()

	t.Run("include_on_record_with_empty_fk_does_not_panic", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}, Attachment{}},
		})
		id := srv.MustID(srv.POST("/attachments", map[string]any{"name": "f.pdf", "user_id": ""}))
		srv.GET("/attachments/" + id + "?include=user").AssertStatus(http.StatusOK)
	})

	t.Run("include_list_with_mixed_fk_presence_returns_both_records", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}, Attachment{}},
		})
		uid := srv.MustID(srv.CreateUser("U", "nfk@x.com", "viewer"))
		srv.MustID(srv.POST("/attachments", map[string]any{"name": "withFK", "user_id": uid}))
		srv.MustID(srv.POST("/attachments", map[string]any{"name": "noFK", "user_id": ""}))

		items := srv.GET("/attachments?include=user").DataList()
		testutil.AssertLen(t, "both attachments returned", items, 2)
	})

	t.Run("include_with_valid_fk_embeds_related_object", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}, Attachment{}},
		})
		uid := srv.MustID(srv.CreateUser("U", "vfk@x.com", "viewer"))
		id := srv.MustID(srv.POST("/attachments", map[string]any{"name": "doc", "user_id": uid}))
		data := srv.GET("/attachments/" + id + "?include=user").Data()
		if _, ok := data["user"].(map[string]any); !ok {
			t.Error("attachment with valid FK must have embedded user object")
		}
	})

	t.Run("empty_fk_does_not_embed_stale_user_object", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{testutil.User{}, Attachment{}},
		})
		id := srv.MustID(srv.POST("/attachments", map[string]any{"name": "orphan", "user_id": ""}))
		data := srv.GET("/attachments/" + id + "?include=user").Data()
		if user, ok := data["user"].(map[string]any); ok {
			if uid := testutil.Field(t, user, "id"); uid != "" {
				t.Errorf("empty FK must not embed user with id=%q", uid)
			}
		}
	})
}

// ── Test infrastructure ───────────────────────────────────────────────────────

// captureSlogHandler captures slog.Records for level assertions.
type captureSlogHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
	groups  []string
}

func (h *captureSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *captureSlogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	newRec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	for _, a := range h.attrs {
		newRec.AddAttrs(a)
	}
	if len(h.groups) > 0 {
		var grouped []slog.Attr
		r.Attrs(func(a slog.Attr) bool {
			grouped = append(grouped, a)
			return true
		})
		for i := len(h.groups) - 1; i >= 0; i-- {
			grouped = []slog.Attr{{
				Key:   h.groups[i],
				Value: slog.GroupValue(grouped...),
			}}
		}
		newRec.AddAttrs(grouped...)
	} else {
		r.Attrs(func(a slog.Attr) bool {
			newRec.AddAttrs(a)
			return true
		})
	}

	*h.records = append(*h.records, newRec)
	return nil
}

func (h *captureSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &captureSlogHandler{
		mu:      h.mu,
		records: h.records,
		attrs:   newAttrs,
		groups:  append([]string{}, h.groups...),
	}
}

func (h *captureSlogHandler) WithGroup(name string) slog.Handler {
	newGroups := append([]string{}, h.groups...)
	newGroups = append(newGroups, name)
	return &captureSlogHandler{
		mu:      h.mu,
		records: h.records,
		attrs:   append([]slog.Attr{}, h.attrs...),
		groups:  newGroups,
	}
}

// chanEventBus sends events to a buffered channel (implements events.Publisher).
type chanEventBus struct{ ch chan<- events.Event }

func (b *chanEventBus) Publish(_ context.Context, e events.Event) error {
	b.ch <- e
	return nil
}
func (b *chanEventBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for _, e := range es {
		_ = b.Publish(ctx, e)
	}
	return nil
}
func (b *chanEventBus) Close() error { return nil }

// countingEventBus counts Publish calls with an atomic counter (implements events.Publisher).
type countingEventBus struct{ n *int64 }

func (b *countingEventBus) Publish(_ context.Context, _ events.Event) error {
	atomic.AddInt64(b.n, 1)
	return nil
}
func (b *countingEventBus) PublishBatch(ctx context.Context, es []events.Event) error {
	for range es {
		atomic.AddInt64(b.n, 1)
	}
	return nil
}
func (b *countingEventBus) Close() error { return nil }

// chanCache2 sends deleted keys to a buffered channel.
// Named with a "2" suffix to avoid a duplicate-symbol error alongside regression_test.go.
type chanCache2 struct{ ch chan<- string }

func (c *chanCache2) Get(context.Context, string) (any, bool)         { return nil, false }
func (c *chanCache2) Set(context.Context, string, any, time.Duration) {}
func (c *chanCache2) Delete(_ context.Context, key string)            { c.ch <- key }

// countingCache2 counts Delete calls atomically.
type countingCache2 struct{ n *int64 }

func (c *countingCache2) Get(context.Context, string) (any, bool)         { return nil, false }
func (c *countingCache2) Set(context.Context, string, any, time.Duration) {}
func (c *countingCache2) Delete(_ context.Context, _ string)              { atomic.AddInt64(c.n, 1) }

// countingQueryCache is a type-preserving CacheStore that counts Get/Set/hit
// calls so CacheQuery tests can assert miss-then-hit behaviour.
type countingQueryCache struct {
	mu               sync.Mutex
	data             map[string]any
	gets, sets, hits int
}

func newCountingQueryCache() *countingQueryCache {
	return &countingQueryCache{data: map[string]any{}}
}

func (c *countingQueryCache) Get(_ context.Context, key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	v, ok := c.data[key]
	if ok {
		c.hits++
	}
	return v, ok
}

func (c *countingQueryCache) Set(_ context.Context, key string, val any, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	c.data[key] = val
}

func (c *countingQueryCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
}

func (c *countingQueryCache) GetCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.gets }
func (c *countingQueryCache) SetCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.sets }
func (c *countingQueryCache) HitCount() int { c.mu.Lock(); defer c.mu.Unlock(); return c.hits }

// badListCache always returns a bare map on Get — the shape a naive
// JSON-decoding store would hand back for a list — so CacheQuery's type guard
// must treat the hit as a miss rather than panic the Response step.
type badListCache struct{ sets int64 }

func (c *badListCache) Get(context.Context, string) (any, bool) {
	return map[string]any{"not": "a-list-result"}, true
}
func (c *badListCache) Set(context.Context, string, any, time.Duration) { atomic.AddInt64(&c.sets, 1) }
func (c *badListCache) Delete(context.Context, string)                  {}

// drainExtChan reads up to n events from ch within the deadline.
// Named to avoid collision with drainChan helpers in other test files.
func drainExtChan(ch <-chan events.Event, n int, deadline time.Duration) []events.Event {
	var out []events.Event
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(out) < n {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-timer.C:
			return out
		}
	}
	return out
}
