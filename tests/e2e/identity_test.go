package e2e

// identity_test.go freezes the v1 record-identity contract (GAP-09). Each test
// names the guarantee it pins and the change that would break it, because these
// assertions exist to fail when the identity model drifts — not to describe how
// any one code path happens to work today.
//
// The contract itself is documented in docs/src/defining-your-api/identity.md.
//
// Run this group:
//
//	go test ./tests/e2e/... -run TestIdentityContract

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// uuidV4 matches the canonical form the framework promises: 8-4-4-4-12 hex, the
// version nibble fixed at 4 and the variant nibble in [89ab] (RFC 4122).
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestIdentityContract(t *testing.T) {
	t.Parallel()

	// ── Generation ────────────────────────────────────────────────────────────

	t.Run("generated_id_is_a_lowercase_v4_uuid", func(t *testing.T) {
		// Breaks if id generation moves to UUIDv7, ULID, a sequence, or an
		// uppercase/braced encoding. Consumers read this shape off the generated
		// OpenAPI document, so changing it is a contract change.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		id := srv.MustID(srv.CreateUser("Ada", "ada@identity.test", "admin"))
		if !uuidV4.MatchString(id) {
			t.Errorf("generated id %q is not a canonical lowercase UUIDv4", id)
		}
	})

	t.Run("every_row_gets_its_own_id", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		seen := make(map[string]bool, 20)
		for i := range 20 {
			id := srv.MustID(srv.CreateUser(
				"dup", "dup"+string(rune('a'+i))+"@identity.test", "viewer"))
			if seen[id] {
				t.Fatalf("id %q was issued twice", id)
			}
			seen[id] = true
		}
	})

	t.Run("id_is_stable_across_updates", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		user := srv.MustID(srv.CreateUser("Grace", "grace@identity.test", "editor"))

		resp := srv.PATCH("/users/"+user, map[string]any{"name": "Grace H"})
		resp.AssertStatus(http.StatusOK)
		testutil.AssertEqual(t, "id after update", resp.Data()["id"], user)
	})

	// ── Ownership: the client never chooses an id ─────────────────────────────

	t.Run("client_supplied_id_on_create_is_ignored", func(t *testing.T) {
		// Two defences cover this one: the Validate step strips the id column
		// outright, and id is readonly, so the generic readonly rule would strip
		// a client value even without it. Breaks only if both go — which is the
		// point of asserting it here rather than trusting either.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		resp := srv.POST("/users", map[string]any{
			"id":       "client-chosen-id",
			"name":     "Mallory",
			"email":    "mallory@identity.test",
			"password": "secret",
			"role":     "viewer",
		})
		resp.AssertStatus(http.StatusCreated)

		id, _ := resp.Data()["id"].(string)
		if id == "client-chosen-id" {
			t.Fatal("the client chose its own primary key")
		}
		if !uuidV4.MatchString(id) {
			t.Errorf("id %q is not framework-generated", id)
		}
		srv.GET("/users/client-chosen-id").AssertStatus(http.StatusNotFound)
	})

	t.Run("middleware_supplied_id_on_create_is_ignored", func(t *testing.T) {
		// The id strip is unconditional, unlike the other readonly columns: a
		// value the server stamped survives the readonly rule, and the id branch
		// runs first precisely so it does not survive this one. Breaks the moment
		// that branch starts honouring ctx.SetField, which would put primary keys
		// back in the request path.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Middleware: func(s *maniflex.Server) {
				s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
					if ctx.Operation == maniflex.OpCreate {
						ctx.SetField("id", "middleware-chosen-id")
					}
					return next()
				})
			},
		})

		id := srv.MustID(srv.CreateUser("Karen", "karen@identity.test", "viewer"))
		if id == "middleware-chosen-id" {
			t.Fatal("a pipeline middleware chose the primary key")
		}
		if !uuidV4.MatchString(id) {
			t.Errorf("id %q is not framework-generated", id)
		}
	})

	t.Run("client_supplied_id_on_update_is_ignored", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		user := srv.MustID(srv.CreateUser("Edsger", "edsger@identity.test", "viewer"))

		srv.PATCH("/users/"+user, map[string]any{
			"id":   "reassigned-id",
			"name": "Edsger D",
		}).AssertStatus(http.StatusOK)

		srv.GET("/users/" + user).AssertStatus(http.StatusOK)
		srv.GET("/users/reassigned-id").AssertStatus(http.StatusNotFound)
	})

	// ── Ids are opaque strings on the wire ────────────────────────────────────

	t.Run("unknown_id_is_404_whatever_its_shape", func(t *testing.T) {
		// Nothing parses or validates the path id — it is bound as a string and
		// matches no row. Breaks if a UUID parse is added to routing, which would
		// turn these into 400s and make natural-key ids unroutable.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		for _, id := range []string{
			"00000000-0000-4000-8000-000000000000", // well-formed, absent
			"not-a-uuid",
			"42",
			"a_natural_key",
		} {
			resp := srv.GET("/users/" + id)
			if resp.Status != http.StatusNotFound {
				t.Errorf("GET /users/%s: got %d, want 404\nbody: %s", id, resp.Status, resp.Body)
			}
		}
	})

	// ── Server-side assignment: supported below the pipeline ──────────────────

	t.Run("server_side_id_is_honoured_below_the_pipeline", func(t *testing.T) {
		// The adapter generates an id only when none was supplied, which is what
		// makes maniflex.SingletonID's fixed row possible. Breaks if generation
		// becomes unconditional. Callers that use this own uniqueness themselves.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		mfx := srv.ManiflexServer()
		bg := maniflex.NewBackground(t.Context(), mfx.DB(), mfx.Registry())

		row, err := bg.GetModel("User").Create(map[string]any{
			"id":       "natural-key-ada",
			"name":     "Ada",
			"email":    "ada.natural@identity.test",
			"password": "secret",
			"role":     "admin",
		})
		if err != nil {
			t.Fatalf("accessor create: %v", err)
		}
		testutil.AssertEqual(t, "stored id", row["id"], "natural-key-ada")

		// And it is routable like any other id.
		srv.GET("/users/natural-key-ada").AssertStatus(http.StatusOK).
			AssertJSON(func(body map[string]any) {
				data, _ := body["data"].(map[string]any)
				testutil.AssertEqual(t, "fetched id", data["id"], "natural-key-ada")
			})
	})

	// ── Relations key off the id string ───────────────────────────────────────

	t.Run("foreign_keys_carry_the_id_string", func(t *testing.T) {
		// Relations are resolved by string equality between the FK column and the
		// target's id. Breaks if identity gains a type the FK column cannot hold.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{})
		user := srv.MustID(srv.CreateUser("Linus", "linus@identity.test", "editor"))
		post := srv.MustID(srv.CreatePost("Relations", "draft", user))

		resp := srv.GET("/posts/" + post + "?include=user").AssertStatus(http.StatusOK)
		data := resp.Data()
		testutil.AssertEqual(t, "user_id", data["user_id"], user)
		included, ok := data["user"].(map[string]any)
		if !ok {
			t.Fatalf("?include=user did not resolve the relation: %v", data)
		}
		testutil.AssertEqual(t, "included user id", included["id"], user)
	})

	// ── Pagination tolerates unordered ids ────────────────────────────────────

	t.Run("cursor_pages_cover_every_row_exactly_once_on_ties", func(t *testing.T) {
		// Keyset pagination orders by (cursor field, id), so the page boundary
		// stays total when rows share a cursor value — every row here carries the
		// same event_at, so the id is the only thing separating them. Random v4
		// ids make that order arbitrary but stable; without the tiebreaker the
		// walk would skip or repeat rows.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Models: []any{TiedEvent{}},
		})

		const total = 9
		const sharedTime = "2026-01-01T00:00:00Z"
		want := make(map[string]bool, total)
		for i := range total {
			resp := srv.POST("/tied_events", map[string]any{
				"name":     "e" + string(rune('a'+i)),
				"event_at": sharedTime,
			})
			want[srv.MustID(resp)] = true
		}

		got := make(map[string]int, total)
		cursor := ""
		pages := 0
		for range total + 2 {
			resp := srv.GET("/tied_events?cursor=" + cursor + "&limit=2").
				AssertStatus(http.StatusOK)
			pages++
			for _, row := range resp.DataList() {
				m, _ := row.(map[string]any)
				id, _ := m["id"].(string)
				got[id]++
			}
			next, _ := resp.Meta()["next_cursor"].(string)
			if next == "" {
				break
			}
			cursor = next
		}

		// Without several pages there are no boundaries to get wrong, and the
		// assertions below would hold for any implementation.
		if pages < 2 {
			t.Fatalf("the walk took %d page(s) — nothing was paginated", pages)
		}
		for id := range want {
			switch got[id] {
			case 1: // exactly once, as required
			case 0:
				t.Errorf("cursor paging skipped row %s", id)
			default:
				t.Errorf("cursor paging returned row %s %d times", id, got[id])
			}
		}
	})

	// ── The published contract ────────────────────────────────────────────────

	t.Run("openapi_declares_ids_as_uuid_strings", func(t *testing.T) {
		// Consumers generate clients from this. Breaks if the id schema or the
		// {id} path parameter stops advertising string/uuid, or if id becomes
		// writable in the generated request bodies.
		t.Parallel()
		srv := testutil.NewServer(t, testutil.Options{
			Config: func(cfg *maniflex.Config) { cfg.Documentation.Public = true },
		})
		resp := srv.GET("/openapi.json").AssertStatus(http.StatusOK)

		var doc map[string]any
		if err := json.Unmarshal(resp.Body, &doc); err != nil {
			t.Fatalf("decode openapi document: %v", err)
		}

		id := oasNode(t, doc, "components", "schemas", "User", "properties", "id")
		testutil.AssertEqual(t, "id type", id["type"], "string")
		testutil.AssertEqual(t, "id format", id["format"], "uuid")
		if readOnly, _ := id["readOnly"].(bool); !readOnly {
			t.Errorf("id must be advertised readOnly — clients never supply it: %v", id)
		}

		item := oasNode(t, doc, "paths", "/users/{id}")
		found := false
		for method, raw := range item {
			op, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			params, _ := op["parameters"].([]any)
			for _, raw := range params {
				p, _ := raw.(map[string]any)
				if p["name"] != "id" || p["in"] != "path" {
					continue
				}
				found = true
				schema, _ := p["schema"].(map[string]any)
				testutil.AssertEqual(t, method+" {id} type", schema["type"], "string")
				testutil.AssertEqual(t, method+" {id} format", schema["format"], "uuid")
			}
		}
		if !found {
			t.Error("/users/{id} declares no id path parameter")
		}
	})

	// ── Registration requires the id column ───────────────────────────────────

	t.Run("model_without_an_id_column_fails_registration", func(t *testing.T) {
		// Single-column string identity is not optional: every generated route,
		// relation, and adapter method is written against it.
		t.Parallel()
		server := maniflex.New(maniflex.Config{
			PathPrefix:         "/api",
			DisableAutoMigrate: true,
		})
		err := server.Register(noIdentityModel{})
		if err == nil {
			t.Fatal("a model with no id column must fail registration")
		}
		if !strings.Contains(err.Error(), "BaseModel") {
			t.Errorf("registration error must name BaseModel, got %q", err)
		}
	})
}

// noIdentityModel deliberately omits maniflex.BaseModel.
type noIdentityModel struct {
	Name string `json:"name" db:"name"`
}

// TiedEvent takes its cursor value from the request so every row can share one,
// leaving the id as the only thing that orders them.
type TiedEvent struct {
	maniflex.BaseModel
	Name    string    `json:"name"     db:"name"     mfx:"required"`
	EventAt time.Time `json:"event_at" db:"event_at" mfx:"required,sortable,cursor_field:event_at"`
}

// oasNode walks a decoded OpenAPI document and fails the test with the path it
// got to when a step is missing.
func oasNode(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	node := doc
	for i, key := range path {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("openapi document has no %v (missing %q, keys: %v)",
				path[:i+1], key, mapKeys(node))
		}
		node = next
	}
	return node
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
