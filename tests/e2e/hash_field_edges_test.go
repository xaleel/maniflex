package e2e

// Audit AUTH-15: service.HashField's edges, on the framework's canonical
// password path. Three of them ended in an account nobody could log into, with
// nothing in the logs saying so.
//
//   - An explicit empty string was treated as "absent" for hashing but left in
//     the body, so PATCH {"password": ""} wrote "" over the stored hash and a
//     create stored "" as the password. mfx:"required" does not help: it means
//     present, not non-empty, and a create with "" returned 201.
//   - A password over bcrypt's 72-BYTE limit was a masked 500 HASH_ERROR -
//     user input reported as a server fault, and alerted on as one. No model tag
//     closes it, because mfx:"maxlen:" counts characters: a 25-character CJK
//     passphrase is 75 bytes.
//   - A hashed field without mfx:"writeonly" is echoed on read, and a client
//     that round-trips the record resubmits the hash as the password, which is
//     hashed again. The real password stops working.
//
//	go test ./tests/e2e/... -run TestHashFieldEdges

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/service"
	svcbcrypt "github.com/xaleel/maniflex/middleware/service/bcrypt"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// hfeUser tags the hashed field as it should be tagged.
type hfeUser struct {
	maniflex.BaseModel
	Email    string `json:"email" db:"email"`
	Password string `json:"password" db:"password" mfx:"writeonly"`
}

// hfeLooseUser omits writeonly, which is the shape that warns.
type hfeLooseUser struct {
	maniflex.BaseModel
	Email    string `json:"email" db:"email"`
	Password string `json:"password" db:"password"`
}

// hfeStore captures what the DB step was about to write.
type hfeStore struct{ last string }

func hfeServer(t *testing.T, store *hfeStore, logs *lockedLog, models ...any) *testutil.Server {
	t.Helper()
	if len(models) == 0 {
		models = []any{hfeUser{}}
	}
	opts := testutil.Options{
		Models: models,
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				service.HashField("password", svcbcrypt.Hasher(4)),
				maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
			s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
				if p, ok := ctx.ParsedBody.Map()["password"].(string); ok {
					store.last = p
				} else {
					store.last = "<absent>"
				}
				return next()
			}, maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate))
		},
	}
	if logs != nil {
		opts.Logger = newLockedLogger(logs)
	}
	return testutil.NewServer(t, opts)
}

// ── (a) the empty string ──────────────────────────────────────────────────────

// On update it means "leave the password alone", which is what an absent field
// already means. A form that submits every field sends a blank password box on
// an ordinary save, so this must not be an error.
func TestHashFieldEdges_EmptyOnUpdateLeavesTheStoredHash(t *testing.T) {
	t.Parallel()
	var store hfeStore
	srv := hfeServer(t, &store, nil)

	id := srv.MustID(srv.POST("/hfe_users", map[string]any{
		"email": "a@x.com", "password": "the-real-password"}))
	hashed := store.last
	if !strings.HasPrefix(hashed, "$2") {
		t.Fatalf("setup: expected a bcrypt hash, got %q", hashed)
	}

	srv.PATCH("/hfe_users/"+id, map[string]any{"email": "a2@x.com", "password": ""}).
		AssertStatus(http.StatusOK)
	if store.last != "<absent>" {
		t.Errorf("the password field reached the DB step as %q; an empty value must "+
			"be removed so the stored hash is left alone", store.last)
	}

	// MUST STILL WORK: a real new password on update is hashed, not skipped.
	srv.PATCH("/hfe_users/"+id, map[string]any{"password": "a-new-password"}).
		AssertStatus(http.StatusOK)
	if !strings.HasPrefix(store.last, "$2") {
		t.Errorf("a real password on update must still be hashed, got %q", store.last)
	}
	if store.last == hashed {
		t.Error("the new password produced the old hash")
	}
}

// On create there is no stored password to leave alone, and an account whose
// password is the empty string can never be logged into. Refused rather than
// stored, which is what used to happen - with a 201.
func TestHashFieldEdges_EmptyOnCreateIsRefused(t *testing.T) {
	t.Parallel()
	var store hfeStore
	resp := hfeServer(t, &store, nil).
		POST("/hfe_users", map[string]any{"email": "b@x.com", "password": ""})
	resp.AssertStatus(http.StatusBadRequest)
	if !strings.Contains(string(resp.Body), "password") {
		t.Errorf("the refusal must name the field: %s", resp.Body)
	}
	if store.last != "" {
		t.Errorf("the request reached the DB step with %q; it must be refused first", store.last)
	}
}

// MUST STILL WORK: an absent field on update is the documented no-op, and must
// not have been turned into the new 400.
func TestHashFieldEdges_AbsentOnUpdateIsStillANoOp(t *testing.T) {
	t.Parallel()
	var store hfeStore
	srv := hfeServer(t, &store, nil)
	id := srv.MustID(srv.POST("/hfe_users", map[string]any{
		"email": "c@x.com", "password": "pw-one"}))

	srv.PATCH("/hfe_users/"+id, map[string]any{"email": "c2@x.com"}).
		AssertStatus(http.StatusOK)
	if store.last != "<absent>" {
		t.Errorf("an absent password must stay absent, got %q", store.last)
	}
}

// ── (b) over the byte limit ───────────────────────────────────────────────────

// 400, not the masked 500 it used to be. Including the cases mfx:"maxlen:72"
// cannot catch, since that tag counts characters and bcrypt counts bytes.
func TestHashFieldEdges_OverLongPasswordIs400(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		pw    string
		chars int
	}{
		{"73 ascii characters", strings.Repeat("x", 73), 73},
		{"25 emoji, 100 bytes", strings.Repeat("\U0001F600", 25), 25},
		{"25 CJK characters, 75 bytes", strings.Repeat("日", 25), 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var store hfeStore
			resp := hfeServer(t, &store, nil).
				POST("/hfe_users", map[string]any{"email": "d@x.com", "password": tc.pw})
			resp.AssertStatus(http.StatusBadRequest)

			body := string(resp.Body)
			// A 400 is not masked, so the reason reaches the client. It must say
			// something actionable rather than "bad request".
			if !strings.Contains(body, "72") {
				t.Errorf("the refusal should name the limit so the client can act on "+
					"it: %s", body)
			}
			if strings.Contains(body, "internal server error") {
				t.Errorf("still reported as a server fault: %s", body)
			}
			// Sanity: these really are short in characters, which is the point.
			if len([]rune(tc.pw)) != tc.chars {
				t.Errorf("setup: %d characters, expected %d", len([]rune(tc.pw)), tc.chars)
			}
		})
	}
}

// MUST STILL WORK: exactly 72 bytes is accepted, so the boundary did not move.
func TestHashFieldEdges_SeventyTwoBytesStillSucceeds(t *testing.T) {
	t.Parallel()
	var store hfeStore
	hfeServer(t, &store, nil).POST("/hfe_users", map[string]any{
		"email": "e@x.com", "password": strings.Repeat("x", 72)}).
		AssertStatus(http.StatusCreated)
	if !strings.HasPrefix(store.last, "$2") {
		t.Errorf("expected a bcrypt hash, got %q", store.last)
	}
}

// MUST STILL WORK: a hasher failure that is NOT about the caller's value is
// still a 500. Only errors marked as client input were reclassified.
func TestHashFieldEdges_ServerSideHasherFailureIsStill500(t *testing.T) {
	t.Parallel()
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{hfeUser{}},
		Middleware: func(s *maniflex.Server) {
			s.Pipeline.Service.Register(
				service.HashField("password", func(string) (string, error) {
					return "", errAHasherBug
				}),
				maniflex.ForOperation(maniflex.OpCreate))
		},
	})
	srv.POST("/hfe_users", map[string]any{"email": "f@x.com", "password": "pw"}).
		AssertStatus(http.StatusInternalServerError)
}

// ── (d) the field must not be readable ────────────────────────────────────────

// A hashed field without writeonly is echoed on read, so a round-trip
// resubmits the hash and HashField hashes it again. Warned once, naming the
// model, the field and the fix.
func TestHashFieldEdges_WarnsWhenTheHashedFieldIsReadable(t *testing.T) {
	t.Parallel()
	logs := &lockedLog{}
	var store hfeStore
	srv := hfeServer(t, &store, logs, hfeLooseUser{})

	srv.POST("/hfe_loose_users", map[string]any{"email": "g@x.com", "password": "pw-one"}).
		AssertStatus(http.StatusCreated)

	got := logs.String()
	if !strings.Contains(got, "returned in API responses") {
		t.Fatalf("no warning for a readable hashed field; logs:\n%s", got)
	}
	for _, want := range []string{"hfeLooseUser", "password", "writeonly"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning omits %q, so it is not actionable; logs:\n%s", want, got)
		}
	}

	// Once per model and field, not once per request: a warning on the hot path
	// would be noise an operator learns to filter.
	before := strings.Count(got, "returned in API responses")
	for range 3 {
		srv.POST("/hfe_loose_users", map[string]any{"email": "h@x.com", "password": "pw"})
	}
	if after := strings.Count(logs.String(), "returned in API responses"); after != before {
		t.Errorf("warned %d times after three more requests, want %d", after, before)
	}
}

// MUST STILL WORK: a correctly tagged field is silent. A warning that fires on
// a correct configuration is one nobody reads.
func TestHashFieldEdges_CorrectlyTaggedFieldIsSilent(t *testing.T) {
	t.Parallel()
	logs := &lockedLog{}
	var store hfeStore
	hfeServer(t, &store, logs, hfeUser{}).
		POST("/hfe_users", map[string]any{"email": "i@x.com", "password": "pw"}).
		AssertStatus(http.StatusCreated)

	if got := logs.String(); strings.Contains(got, "returned in API responses") {
		t.Errorf("warned about a field already tagged writeonly; logs:\n%s", got)
	}
}

// errAHasherBug stands for a server-side hasher failure: not the caller's
// value, so it must still be a 500.
var errAHasherBug = errors.New("the hasher is broken")
