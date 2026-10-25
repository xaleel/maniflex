package e2e_test

// Audit AUTH-19: RequireOwner answered 404 to the record's own owner whenever
// the owner column was nullable.
//
// The comparison rendered the stored value with fmt.Sprint, and the map the
// accessor returns holds each struct field as it stands — so OwnerID *string,
// which is how models.md says to declare an optional owner, arrived as a
// *string and rendered as a hex address. Nothing could equal that, so every
// read, update and delete 404'd. It fails closed, so no data leaked; the
// middleware was simply non-functional on such a model, and the 404 reads as
// "no such record", so nothing pointed at the cause.
//
// The non-pointer sibling below is the control: it passed throughout, which is
// why no existing test caught this.
//
//	go test ./tests/e2e/... -run TestOwnerNullable

import (
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Nullable owner — a pointer field, per models.md.
type nullableOwnerDoc struct {
	maniflex.BaseModel
	Title   string  `json:"title" db:"title"`
	OwnerID *string `json:"owner_id" db:"owner_id"`
}

// Non-nullable owner — the shape that always worked.
type stringOwnerDoc struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID string `json:"owner_id" db:"owner_id"`
}

// Numeric nullable owner, for an issuer minting an integer sub.
type numericOwnerDoc struct {
	maniflex.BaseModel
	Title   string `json:"title" db:"title"`
	OwnerID *int64 `json:"owner_id" db:"owner_id"`
}

const ownerSecret = "an-owner-test-secret-of-32-bytes!"

func ownerToken(t *testing.T, sub any) string {
	t.Helper()
	return signHMAC(t, "HS256", ownerSecret, map[string]any{
		"sub": sub, "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func ownerServer(t *testing.T, adminRoles ...string) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			// History is mounted on the two the history assertions use: the
			// derived reads are half of what AUTH-2 brought under this check.
			nullableOwnerDoc{}, maniflex.ModelConfig{Versioned: true},
			stringOwnerDoc{}, maniflex.ModelConfig{Versioned: true},
			numericOwnerDoc{},
		},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Auth.Register(auth.JWTAuth(ownerSecret))
			s.Pipeline.Auth.Register(auth.RequireOwner("owner_id", adminRoles...))
		},
	})
}

func asUser(t *testing.T, sub any) map[string]string {
	t.Helper()
	return map[string]string{"Authorization": "Bearer " + ownerToken(t, sub)}
}

// The whole lifecycle an owner must be able to drive on their own record,
// across both column shapes. The pointer column 404'd on all three verbs.
func TestOwnerNullable_OwnerCanUseTheirOwnRecord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, path string }{
		{"nullable", "/nullable_owner_docs"},
		{"non_nullable", "/string_owner_docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			s := ownerServer(t)
			alice := asUser(t, "alice")

			created := s.POST(path, map[string]any{"title": "mine"}, alice)
			created.AssertStatus(http.StatusCreated)
			if got := created.Data()["owner_id"]; got != "alice" {
				t.Fatalf("owner_id on create: got %v, want alice", got)
			}
			id := created.ID()

			s.GET(path+"/"+id, alice).AssertStatus(http.StatusOK)
			s.PATCH(path+"/"+id, map[string]any{"title": "renamed"}, alice).
				AssertStatus(http.StatusOK)
			s.GET(path+"/"+id+"/history", alice).AssertStatus(http.StatusOK)
			s.DELETE(path+"/"+id, alice).AssertStatus(http.StatusNoContent)
		})
	}
}

// MUST STILL WORK: the refusal the middleware exists for. Dereferencing must not
// have turned the comparison into something a stranger can satisfy — and 404,
// not 403, so the endpoint does not confirm the record exists.
func TestOwnerNullable_StrangerIsStillRefused(t *testing.T) {
	t.Parallel()
	s := ownerServer(t)
	alice, mallory := asUser(t, "alice"), asUser(t, "mallory")

	id := s.MustID(s.POST("/nullable_owner_docs", map[string]any{"title": "mine"}, alice))

	for _, tc := range []struct {
		name string
		do   func() *testutil.Response
	}{
		{"read", func() *testutil.Response { return s.GET("/nullable_owner_docs/"+id, mallory) }},
		{"update", func() *testutil.Response {
			return s.PATCH("/nullable_owner_docs/"+id, map[string]any{"title": "stolen"}, mallory)
		}},
		{"history", func() *testutil.Response {
			return s.GET("/nullable_owner_docs/"+id+"/history", mallory)
		}},
		{"delete", func() *testutil.Response { return s.DELETE("/nullable_owner_docs/"+id, mallory) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := tc.do()
			resp.AssertStatus(http.StatusNotFound)
			if got := resp.ErrorCode(); got != "NOT_FOUND" {
				t.Errorf("error code: got %s, want NOT_FOUND", got)
			}
		})
	}

	// And the record is untouched: the refusals were not cosmetic.
	if got := s.GET("/nullable_owner_docs/"+id, alice).Data()["title"]; got != "mine" {
		t.Errorf("title: got %v, want mine", got)
	}
}

// A row whose owner column is NULL belongs to nobody, so it stays 404 to every
// non-admin — unchanged by the fix, and the fail-closed direction. A nil pointer
// renders as "", and RequireOwner refuses an empty UserID before it ever gets
// here, so "" can never match a caller.
func TestOwnerNullable_UnownedRecordIsRefused(t *testing.T) {
	t.Parallel()
	s := ownerServer(t, "admin")
	admin := map[string]string{"Authorization": "Bearer " + signHMAC(t, "HS256", ownerSecret,
		map[string]any{"sub": "root", "roles": []string{"admin"}, "exp": time.Now().Add(time.Hour).Unix()})}

	// Only an admin can create one: RequireOwner stamps the caller otherwise.
	id := s.MustID(s.POST("/nullable_owner_docs", map[string]any{"title": "orphan"}, admin))

	s.GET("/nullable_owner_docs/"+id, asUser(t, "alice")).AssertStatus(http.StatusNotFound)
	// MUST STILL WORK: the admin bypass does not consult the owner at all.
	s.GET("/nullable_owner_docs/"+id, admin).AssertStatus(http.StatusOK)

	// The numeric column is where "" has to mean absent rather than the
	// pointee's zero value. Rendering a nil *int64 as "0" would hand every
	// unowned row to whichever caller the issuer numbered 0 — which is a real
	// subject, since JWTAuth reads a numeric sub as its exact decimal.
	zero := s.MustID(s.POST("/numeric_owner_docs", map[string]any{"title": "orphan"}, admin))
	s.GET("/numeric_owner_docs/"+zero, asUser(t, 0)).AssertStatus(http.StatusNotFound)
}

// Dereferencing covers a numeric owner too: *int64 renders as its decimal,
// which is the exact form JWTAuth gives an integer sub (audit AUTH-5).
func TestOwnerNullable_NumericOwnerMatches(t *testing.T) {
	t.Parallel()
	s := ownerServer(t)
	user42 := asUser(t, 42)

	created := s.POST("/numeric_owner_docs", map[string]any{"title": "mine"}, user42)
	created.AssertStatus(http.StatusCreated)
	id := created.ID()

	s.GET("/numeric_owner_docs/"+id, user42).AssertStatus(http.StatusOK)
	s.PATCH("/numeric_owner_docs/"+id, map[string]any{"title": "renamed"}, user42).
		AssertStatus(http.StatusOK)

	// MUST STILL WORK: a different number is a different owner.
	s.GET("/numeric_owner_docs/"+id, asUser(t, 43)).AssertStatus(http.StatusNotFound)
}

// MUST STILL WORK: the update stamp (audit AUTH-4). An owner must not be able to
// PATCH the owner column to hand the record away, on a nullable column either.
func TestOwnerNullable_OwnerCannotTransferByPatch(t *testing.T) {
	t.Parallel()
	s := ownerServer(t)
	alice := asUser(t, "alice")

	id := s.MustID(s.POST("/nullable_owner_docs", map[string]any{"title": "mine"}, alice))
	updated := s.PATCH("/nullable_owner_docs/"+id,
		map[string]any{"owner_id": "mallory"}, alice)
	updated.AssertStatus(http.StatusOK)
	if got := updated.Data()["owner_id"]; got != "alice" {
		t.Errorf("owner_id: got %v, want alice — the stamp must survive the body", got)
	}

	// And it really is still alice's, not just in the response body.
	s.GET("/nullable_owner_docs/"+id, asUser(t, "mallory")).AssertStatus(http.StatusNotFound)
	s.GET("/nullable_owner_docs/"+id, alice).AssertStatus(http.StatusOK)
}
