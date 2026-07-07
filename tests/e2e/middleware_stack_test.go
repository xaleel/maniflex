package e2e

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/middleware/db"
	"github.com/xaleel/maniflex/middleware/response"
	"github.com/xaleel/maniflex/middleware/service"
	svcbcrypt "github.com/xaleel/maniflex/middleware/service/bcrypt"
	"github.com/xaleel/maniflex/middleware/validate"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TestMiddlewareStack tests the provided middleware packages end-to-end
// through the full HTTP server.
func TestMiddlewareStack(t *testing.T) {
	t.Parallel()

	// ── auth.JWTAuth ──────────────────────────────────────────────────────────

	t.Run("jwt_auth_valid_token_passes", func(t *testing.T) {
		t.Parallel()
		secret := "test-secret-key"
		token := makeJWT(t, secret, "user-1", []string{"admin"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "a@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		)
		resp.AssertStatus(http.StatusCreated)
	})

	t.Run("jwt_auth_missing_token_returns_401", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("secret"),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users", map[string]any{"name": "A", "email": "a@x.com", "password": "s"})
		resp.AssertStatus(http.StatusUnauthorized)
	})

	t.Run("jwt_auth_bad_signature_returns_401", func(t *testing.T) {
		t.Parallel()
		token := makeJWT(t, "other-secret", "user-1", nil, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("correct-secret"),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "a@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		)
		resp.AssertStatus(http.StatusUnauthorized)
	})

	t.Run("jwt_auth_expired_token_returns_401", func(t *testing.T) {
		t.Parallel()
		token := makeJWT(t, "secret", "user-1", nil, -time.Minute)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("secret"),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "a@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		)
		resp.AssertStatus(http.StatusUnauthorized)
	})

	t.Run("jwt_auth_populates_ctx_auth_user_id", func(t *testing.T) {
		t.Parallel()
		secret := "test-secret"
		token := makeJWT(t, secret, "captured-user", []string{"editor"}, time.Hour)
		var capturedUserID string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Auth != nil {
						mu.Lock()
						capturedUserID = ctx.Auth.UserID
						mu.Unlock()
					}
					return next()
				})
			},
		})
		srv.GET("/users", map[string]string{"Authorization": "Bearer " + token})

		mu.Lock()
		got := capturedUserID
		mu.Unlock()
		testutil.AssertEqual(t, "captured user ID", got, "captured-user")
	})

	t.Run("jwt_auth_reads_roles_from_token", func(t *testing.T) {
		t.Parallel()
		secret := "s"
		token := makeJWT(t, secret, "u", []string{"admin", "editor"}, time.Hour)
		var capturedRoles []string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				s.Pipeline.Service.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Auth != nil {
						mu.Lock()
						capturedRoles = append(capturedRoles, ctx.Auth.Roles...)
						mu.Unlock()
					}
					return next()
				})
			},
		})
		srv.GET("/users", map[string]string{"Authorization": "Bearer " + token})

		mu.Lock()
		roles := make([]string, len(capturedRoles))
		copy(roles, capturedRoles)
		mu.Unlock()
		testutil.AssertContains(t, "admin role", roles, "admin")
		testutil.AssertContains(t, "editor role", roles, "editor")
	})

	t.Run("jwt_auth_rs256_valid_token_passes", func(t *testing.T) {
		t.Parallel()
		priv := mustRSAKey(t)
		token := makeAsymJWT(t, "RS256", priv, "rsa-user", []string{"admin"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("", auth.JWTOptions{PublicKey: &priv.PublicKey}),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "R", "email": "rs256@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		).AssertStatus(http.StatusCreated)
	})

	t.Run("jwt_auth_rs256_wrong_key_returns_401", func(t *testing.T) {
		t.Parallel()
		signing := mustRSAKey(t)
		other := mustRSAKey(t)
		token := makeAsymJWT(t, "RS256", signing, "u", nil, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("", auth.JWTOptions{PublicKey: &other.PublicKey}),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "R", "email": "rs256bad@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		).AssertStatus(http.StatusUnauthorized)
	})

	t.Run("jwt_auth_rs256_rejects_hs256_alg_confusion", func(t *testing.T) {
		t.Parallel()
		// An attacker tries to forge an HS256 token using the public key bytes
		// as the HMAC secret. With PublicKey set we must refuse HS256 entirely.
		priv := mustRSAKey(t)
		hsToken := makeJWT(t, "anything", "u", nil, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("anything", auth.JWTOptions{PublicKey: &priv.PublicKey}),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "R", "email": "alg@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + hsToken},
		).AssertStatus(http.StatusUnauthorized)
	})

	t.Run("jwt_auth_es256_valid_token_passes", func(t *testing.T) {
		t.Parallel()
		priv := mustECDSAKey(t, elliptic.P256())
		token := makeAsymJWT(t, "ES256", priv, "ec-user", []string{"admin"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("", auth.JWTOptions{PublicKey: &priv.PublicKey}),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "E", "email": "es256@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		).AssertStatus(http.StatusCreated)
	})

	t.Run("jwt_auth_es256_wrong_key_returns_401", func(t *testing.T) {
		t.Parallel()
		signing := mustECDSAKey(t, elliptic.P256())
		other := mustECDSAKey(t, elliptic.P256())
		token := makeAsymJWT(t, "ES256", signing, "u", nil, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth("", auth.JWTOptions{PublicKey: &other.PublicKey}),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "E", "email": "es256bad@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		).AssertStatus(http.StatusUnauthorized)
	})

	// ── auth.APIKeyAuth ───────────────────────────────────────────────────────

	t.Run("api_key_auth_valid_key_passes", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
					auth.APIKeyEntry{
						Key:  "valid-key",
						Auth: maniflex.AuthInfo{UserID: "svc-1", Roles: []string{"admin"}},
					},
				), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "apik@x.com", "password": "s"},
			map[string]string{"X-API-Key": "valid-key"},
		)
		resp.AssertStatus(http.StatusCreated)
	})

	t.Run("api_key_auth_unknown_key_returns_401", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
					auth.APIKeyEntry{Key: "valid", Auth: maniflex.AuthInfo{}},
				), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "bad@x.com", "password": "s"},
			map[string]string{"X-API-Key": "wrong-key"},
		)
		resp.AssertStatus(http.StatusUnauthorized)
	})

	t.Run("api_key_auth_missing_header_returns_401", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.APIKeyAuth("X-API-Key",
					auth.APIKeyEntry{Key: "k", Auth: maniflex.AuthInfo{}},
				), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users", map[string]any{"name": "A", "email": "miss@x.com", "password": "s"})
		resp.AssertStatus(http.StatusUnauthorized)
	})

	// ── auth.RequireRole ──────────────────────────────────────────────────────

	t.Run("require_role_allows_matching_role", func(t *testing.T) {
		t.Parallel()
		secret := "s"
		token := makeJWT(t, secret, "u", []string{"admin"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				s.Pipeline.Auth.Register(auth.RequireRole("admin"),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "rr@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		)
		resp.AssertStatus(http.StatusCreated)
	})

	t.Run("require_role_blocks_wrong_role", func(t *testing.T) {
		t.Parallel()
		secret := "s"
		viewerToken := makeJWT(t, secret, "viewer-u", []string{"viewer"}, time.Hour)
		adminToken := makeJWT(t, secret, "admin-u", []string{"admin"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				s.Pipeline.Auth.Register(auth.RequireRole("admin"),
					maniflex.ForOperation(maniflex.OpDelete))
			},
		})
		// Create without role restriction
		id := srv.MustID(srv.POST("/users",
			map[string]any{"name": "A", "email": "rr2@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + adminToken},
		))
		// Viewer cannot delete
		srv.DELETE("/users/"+id,
			map[string]string{"Authorization": "Bearer " + viewerToken},
		).AssertStatus(http.StatusForbidden)
		// Admin can delete
		srv.DELETE("/users/"+id,
			map[string]string{"Authorization": "Bearer " + adminToken},
		).AssertStatus(http.StatusNoContent)
	})

	t.Run("require_role_or_semantics_any_matching_role_passes", func(t *testing.T) {
		t.Parallel()
		secret := "s"
		editorToken := makeJWT(t, secret, "e", []string{"editor"}, time.Hour)

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				// admin OR editor can create
				s.Pipeline.Auth.Register(auth.RequireRole("admin", "editor"),
					maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		resp := srv.POST("/users",
			map[string]any{"name": "A", "email": "rror@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + editorToken},
		)
		resp.AssertStatus(http.StatusCreated)
	})

	// ── auth.BlockOperation ───────────────────────────────────────────────────

	t.Run("block_operation_returns_405_for_blocked_op", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpCreate, maniflex.OpDelete),
					maniflex.ForModel("Tag"))
			},
		})
		srv.POST("/tags", map[string]any{"name": "Go", "color": "blue"}).
			AssertStatus(http.StatusMethodNotAllowed)
	})

	t.Run("block_operation_does_not_affect_other_models", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpCreate),
					maniflex.ForModel("Tag"))
			},
		})
		// User create still works
		srv.POST("/users", map[string]any{"name": "U", "email": "bo@x.com", "password": "s"}).
			AssertStatus(http.StatusCreated)
	})

	t.Run("block_operation_allows_unblocked_ops_on_same_model", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.BlockOperation(maniflex.OpDelete),
					maniflex.ForModel("User"))
			},
		})
		// Create and read still work; delete blocked
		id := srv.MustID(srv.CreateUser("U", "boua@x.com", "viewer"))
		srv.GET("/users/" + id).AssertStatus(http.StatusOK)
		srv.DELETE("/users/" + id).AssertStatus(http.StatusMethodNotAllowed)
	})

	// ── service.HashField ─────────────────────────────────────────────────────

	t.Run("hash_field_output_is_not_plaintext", func(t *testing.T) {
		t.Parallel()
		var storedPassword string
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.HashField("password", svcbcrypt.Hasher()),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
				// Capture what actually gets written to the DB
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					if p, ok := ctx.ParsedBody.Map()["password"].(string); ok {
						storedPassword = p
					}
					mu.Unlock()
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users", map[string]any{
			"name": "A", "email": "hf@x.com", "password": "plaintext123",
		})

		mu.Lock()
		stored := storedPassword
		mu.Unlock()

		if stored == "plaintext123" {
			t.Error("password must be hashed before DB write")
		}
		if stored == "" {
			t.Error("password field must be present in DB data")
		}
		if !strings.HasPrefix(stored, "$2") {
			t.Errorf("expected bcrypt hash (starts with $2), got: %s", stored)
		}
	})

	t.Run("hash_field_absent_on_patch_does_not_hash_empty", func(t *testing.T) {
		t.Parallel()
		var patchBody map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.HashField("password", svcbcrypt.Hasher()),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate),
				)
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpUpdate {
						mu.Lock()
						patchBody = ctx.ParsedBody.Map()
						mu.Unlock()
					}
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpUpdate))
			},
		})
		id := srv.MustID(srv.POST("/users", map[string]any{
			"name": "A", "email": "hfp@x.com", "password": "original",
		}))
		srv.PATCH("/users/"+id, map[string]any{"name": "B"}).AssertStatus(http.StatusOK)

		mu.Lock()
		body := patchBody
		mu.Unlock()

		if _, hasPassword := body["password"]; hasPassword {
			t.Error("PATCH without password field must not inject hashed empty string")
		}
	})

	// ── service.SetField ──────────────────────────────────────────────────────

	t.Run("set_field_injects_value_into_body", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Service.Register(
					service.SetField("role", func(_ *maniflex.ServerContext) any { return "editor" }),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		resp := srv.POST("/users", map[string]any{
			"name": "A", "email": "sf@x.com", "password": "s",
		})
		resp.AssertStatus(http.StatusCreated)
		testutil.AssertEqual(t, "injected role", testutil.Field(t, resp.Data(), "role"), "editor")
	})

	t.Run("set_field_can_read_from_ctx_auth", func(t *testing.T) {
		t.Parallel()
		secret := "s"
		token := makeJWT(t, secret, "auth-owner", nil, time.Hour)
		var capturedBody map[string]any
		var mu sync.Mutex

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(auth.JWTAuth(secret))
				s.Pipeline.Service.Register(
					service.SetField("role", func(ctx *maniflex.ServerContext) any {
						if ctx.Auth != nil {
							return "injected"
						}
						return "no-auth"
					}),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
				s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					mu.Lock()
					capturedBody = ctx.ParsedBody.Map()
					mu.Unlock()
					return next()
				}, maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate))
			},
		})
		srv.POST("/users",
			map[string]any{"name": "A", "email": "sfca@x.com", "password": "s"},
			map[string]string{"Authorization": "Bearer " + token},
		)

		mu.Lock()
		body := capturedBody
		mu.Unlock()
		testutil.AssertEqual(t, "role from auth", fmt.Sprintf("%v", body["role"]), "injected")
	})

	// ── validate.ForbiddenValues ──────────────────────────────────────────────

	t.Run("forbidden_values_rejects_disallowed_value", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.ForbiddenValues("role", "admin"),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpCreate),
				)
			},
		})
		srv.POST("/users", map[string]any{
			"name": "H", "email": "fv@x.com", "password": "s", "role": "admin",
		}).AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("forbidden_values_allows_permitted_values", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.ForbiddenValues("role", "admin"),
					maniflex.ForModel("User"),
				)
			},
		})
		srv.POST("/users", map[string]any{
			"name": "A", "email": "fva@x.com", "password": "s", "role": "editor",
		}).AssertStatus(http.StatusCreated)
	})

	// ── validate.RequireAtLeastOne ────────────────────────────────────────────

	t.Run("require_at_least_one_fails_when_all_absent", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RequireAtLeastOne("name", "role"),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpUpdate),
				)
			},
		})
		id := srv.MustID(srv.CreateUser("A", "rao@x.com", "viewer"))
		srv.PATCH("/users/"+id, map[string]any{"score": 5}).AssertStatus(http.StatusUnprocessableEntity)
	})

	t.Run("require_at_least_one_passes_when_one_present", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Validate.Register(
					validate.RequireAtLeastOne("name", "role"),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpUpdate),
				)
			},
		})
		id := srv.MustID(srv.CreateUser("A", "raop@x.com", "viewer"))
		srv.PATCH("/users/"+id, map[string]any{"name": "B"}).AssertStatus(http.StatusOK)
	})

	// ── db.ForceFilter ────────────────────────────────────────────────────────

	t.Run("force_filter_restricts_list_to_matching_records", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					db.ForceFilter("role", func(_ *maniflex.ServerContext) any { return "admin" }),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList),
				)
			},
		})
		srv.MustID(srv.CreateUser("Admin", "admin@x.com", "admin"))
		srv.MustID(srv.CreateUser("Editor", "ed@x.com", "editor"))
		items := srv.GET("/users").DataList()
		testutil.AssertLen(t, "only admins", items, 1)
		testutil.AssertEqual(t, "role", testutil.Field(t, items[0].(map[string]any), "role"), "admin")
	})

	t.Run("force_filter_does_not_affect_other_models", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				// Force User list to admin only — must not affect post list
				s.Pipeline.DB.Register(
					db.ForceFilter("role", func(_ *maniflex.ServerContext) any { return "admin" }),
					maniflex.ForModel("User"), maniflex.ForOperation(maniflex.OpList),
				)
			},
		})
		u := srv.MustID(srv.CreateUser("Admin", "a2@x.com", "admin"))
		srv.MustID(srv.CreatePost("P1", "draft", u))
		srv.MustID(srv.CreatePost("P2", "draft", u))
		// posts list should return all 2
		items := srv.GET("/posts").DataList()
		testutil.AssertLen(t, "all posts", items, 2)
	})

	// ── db.AuditLog ───────────────────────────────────────────────────────────

	t.Run("audit_log_receives_record_after_successful_create", func(t *testing.T) {
		t.Parallel()
		received := make(chan db.AuditRecord, 1)
		sink := &testAuditSink{fn: func(r db.AuditRecord) { received <- r }}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					db.AuditLog(sink),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.MustID(srv.CreateUser("Audited", "audit@x.com", "viewer"))

		select {
		case rec := <-received:
			testutil.AssertEqual(t, "model", rec.Model, "User")
			testutil.AssertEqual(t, "op", string(rec.Operation), "create")
		case <-time.After(500 * time.Millisecond):
			t.Error("audit record not received within 500ms")
		}
	})

	t.Run("audit_log_does_not_fire_when_validation_fails", func(t *testing.T) {
		t.Parallel()
		var count int64
		sink := &testAuditSink{fn: func(_ db.AuditRecord) { atomic.AddInt64(&count, 1) }}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.DB.Register(
					db.AuditLog(sink),
					maniflex.ForOperation(maniflex.OpCreate), maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.POST("/users", map[string]any{}).AssertStatus(http.StatusUnprocessableEntity)
		time.Sleep(100 * time.Millisecond)
		if atomic.LoadInt64(&count) != 0 {
			t.Error("audit sink must not be called when request fails before DB")
		}
	})

	// ── response.CORSHeaders ──────────────────────────────────────────────────

	t.Run("cors_sets_allow_origin_on_get", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.CORSHeaders("test_origin"),
					maniflex.AtPosition(maniflex.Before),
				)
			},
		})
		resp := srv.GET("/users", map[string]string{"Origin": "test_origin"})
		testutil.AssertEqual(t, "Access-Control-Allow-Origin response header", resp.Header.Get("Access-Control-Allow-Origin"), "test_origin")
	})

	t.Run("cors_handles_options_preflight", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(response.CORSHeaders("*"), maniflex.AtPosition(maniflex.Before))
			},
		})
		srv.Do("OPTIONS", srv.APIPath("/users"), nil).AssertStatus(http.StatusOK)
	})

	t.Run("cors_explicit_wildcard_emits_star", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.CORSHeaders("*"),
					maniflex.AtPosition(maniflex.Before),
				)
			},
		})
		resp := srv.GET("/users", map[string]string{"Origin": "https://anything.example"})
		testutil.AssertEqual(t, "wildcard ACAO", resp.Header.Get("Access-Control-Allow-Origin"), "*")
	})

	t.Run("cors_non_allowlisted_origin_not_reflected", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.CORSHeaders("https://allowed.example"),
					maniflex.AtPosition(maniflex.Before),
				)
			},
		})
		resp := srv.GET("/users", map[string]string{"Origin": "https://evil.example"})
		testutil.AssertEqual(t, "non-allowlisted ACAO must be empty",
			resp.Header.Get("Access-Control-Allow-Origin"), "")
	})

	// ── response.AddHeader ────────────────────────────────────────────────────

	t.Run("add_header_is_present_on_every_response", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(response.AddHeader("X-Powered-By", "maniflex"))
			},
		})
		testutil.AssertEqual(t, "GET header", srv.GET("/users").Header.Get("X-Powered-By"), "maniflex")
		resp := srv.POST("/users", map[string]any{"name": "A", "email": "ah@x.com", "password": "s"})
		testutil.AssertEqual(t, "POST header", resp.Header.Get("X-Powered-By"), "maniflex")
	})

	// ── response.TransformField ───────────────────────────────────────────────

	t.Run("transform_field_applied_on_single_read", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.TransformField("name", func(v any) any {
						if s, ok := v.(string); ok {
							return strings.ToUpper(s)
						}
						return v
					}),
					maniflex.ForModel("User"), maniflex.AtPosition(maniflex.After),
				)
			},
		})
		id := srv.MustID(srv.CreateUser("alice", "tf@x.com", "viewer"))
		testutil.AssertEqual(t, "uppercased name",
			testutil.Field(t, srv.GET("/users/"+id).Data(), "name"), "ALICE")
	})

	t.Run("transform_field_applied_to_all_items_in_list", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(
					response.TransformField("name", func(v any) any {
						if s, ok := v.(string); ok {
							return "~" + s
						}
						return v
					}),
					maniflex.ForModel("User"), maniflex.AtPosition(maniflex.After),
				)
			},
		})
		srv.MustID(srv.CreateUser("alice", "tfl1@x.com", "viewer"))
		srv.MustID(srv.CreateUser("bob", "tfl2@x.com", "viewer"))
		for _, item := range srv.GET("/users").DataList() {
			name := testutil.Field(t, item.(map[string]any), "name")
			if !strings.HasPrefix(name, "~") {
				t.Errorf("transform not applied: %q", name)
			}
		}
	})

	// ── response.Metrics ─────────────────────────────────────────────────────

	t.Run("metrics_collector_called_per_request", func(t *testing.T) {
		t.Parallel()
		mc := &testMetricsCollector{}

		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Response.Register(response.Metrics(mc), maniflex.AtPosition(maniflex.After))
			},
		})
		for range 5 {
			srv.GET("/users")
		}
		mc.mu.Lock()
		counter := mc.counterCalls
		histo := mc.histoCalls
		mc.mu.Unlock()

		if counter < 5 {
			t.Errorf("IncCounter called %d times for 5 requests", counter)
		}
		if histo < 5 {
			t.Errorf("ObserveHistogram called %d times for 5 requests", histo)
		}
	})
}

// ── Test infrastructure ───────────────────────────────────────────────────────

type testAuditSink struct {
	fn func(db.AuditRecord)
}

func (s *testAuditSink) Write(_ context.Context, r db.AuditRecord) error {
	s.fn(r)
	return nil
}

type testMetricsCollector struct {
	mu           sync.Mutex
	counterCalls int
	histoCalls   int
}

func (m *testMetricsCollector) IncCounter(_ string, _ map[string]string) {
	m.mu.Lock()
	m.counterCalls++
	m.mu.Unlock()
}

func (m *testMetricsCollector) ObserveHistogram(_ string, _ float64, _ map[string]string) {
	m.mu.Lock()
	m.histoCalls++
	m.mu.Unlock()
}

// makeJWT builds a minimal HS256 JWT that matches what middleware/auth/jwt.go parses.
// This keeps the test package self-contained with no external JWT dependency.
func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func mustECDSAKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	return k
}

// makeAsymJWT signs a JWT using either an *rsa.PrivateKey (RS256/384/512) or
// an *ecdsa.PrivateKey (ES256/384/512) and returns the compact serialisation.
func makeAsymJWT(t *testing.T, alg string, key crypto.Signer, subject string, roles []string, expiry time.Duration) string {
	t.Helper()

	header := base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"alg":%q,"typ":"JWT"}`, alg)))

	claims := map[string]any{
		"sub": subject,
		"exp": time.Now().Add(expiry).Unix(),
		"iat": time.Now().Unix(),
	}
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("makeAsymJWT: marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)
	signingInput := header + "." + payload

	var hashed []byte
	var hashID crypto.Hash
	switch alg {
	case "RS256", "ES256":
		h := sha256.Sum256([]byte(signingInput))
		hashed, hashID = h[:], crypto.SHA256
	default:
		t.Fatalf("makeAsymJWT: unsupported alg %q", alg)
	}

	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, hashID, hashed)
		if err != nil {
			t.Fatalf("rsa sign: %v", err)
		}
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, hashed)
		if err != nil {
			t.Fatalf("ecdsa sign: %v", err)
		}
		keySize := (k.Curve.Params().BitSize + 7) / 8
		sig = make([]byte, 2*keySize)
		rb, sb := r.Bytes(), s.Bytes()
		copy(sig[keySize-len(rb):keySize], rb)
		copy(sig[2*keySize-len(sb):], sb)
	default:
		t.Fatalf("makeAsymJWT: unsupported key %T", key)
	}

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func makeJWT(t *testing.T, secret, subject string, roles []string, expiry time.Duration) string {
	t.Helper()

	header := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"alg":"HS256","typ":"JWT"}`))

	claims := map[string]any{
		"sub": subject,
		"exp": time.Now().Add(expiry).Unix(),
		"iat": time.Now().Unix(),
	}
	if len(roles) > 0 {
		claims["roles"] = roles
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("makeJWT: marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(header + "." + payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return header + "." + payload + "." + sig
}
