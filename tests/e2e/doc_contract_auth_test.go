package e2e_test

// Audit AUTH-16: three reference docs described behaviour the code did not have.
// Each is pinned here by running what the docs say, so the next drift fails a
// test rather than reaching a reader.
//
//   - auth.md promised HMAC "HS256/384/512". Only HS256 verified, so an issuer
//     minting HS512 on that promise got 401 on every request, with "HS256
//     required" as the only clue - while the asymmetric path in the same file
//     already accepted all six of its variants.
//   - service.OwnerScope was documented as an overwrite of whatever the client
//     sent. With no principal it returned next() and stored the client's value
//     verbatim, which is the caller choosing their own owner under the
//     middleware that exists to choose it for them. An empty UserID was already
//     refused, so the two cases answered oppositely.
//   - The catalogue's SetField example was `return ctx.Auth.UserID` with no nil
//     check: a 500 PANIC on any anonymous create. The Go-doc example guarded it
//     and returned nil, which is worse on an update - SetField writes JSON null,
//     so it erased the owner column.
//
//	go test ./tests/e2e/... -run TestDocContractAuth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/middleware/auth"
	"github.com/xaleel/maniflex/middleware/service"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type dcaPost struct {
	maniflex.BaseModel
	Title  string  `json:"title" db:"title"`
	UserID *string `json:"user_id" db:"user_id"`
}

const dcaSecret = "doc-contract-hmac-secret-32-bytes!"

// signHMAC mints a token with any of the three HS algorithms, independently of
// the framework's own signing code, so this tests the contract rather than a
// round-trip through one implementation.
func signHMAC(t *testing.T, alg, secret string, claims map[string]any) string {
	t.Helper()
	var mac hash.Hash
	switch alg {
	case "HS256":
		mac = hmac.New(sha256.New, []byte(secret))
	case "HS384":
		mac = hmac.New(sha512.New384, []byte(secret))
	case "HS512":
		mac = hmac.New(sha512.New, []byte(secret))
	default:
		t.Fatalf("signHMAC: unsupported alg %q", alg)
	}
	hdr := base64.RawURLEncoding.EncodeToString(
		fmt.Appendf(nil, `{"alg":%q,"typ":"JWT"}`, alg))
	cb, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	input := hdr + "." + base64.RawURLEncoding.EncodeToString(cb)
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func dcaServer(t *testing.T, reg func(*maniflex.Server)) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models:     []any{dcaPost{}},
		Middleware: reg,
	})
}

// ── (a) the HMAC family auth.md promises ──────────────────────────────────────

func TestDocContractAuth_AllThreeHMACAlgorithmsVerify(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.JWTAuth(dcaSecret))
	})
	claims := map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}

	for _, alg := range []string{"HS256", "HS384", "HS512"} {
		t.Run(alg, func(t *testing.T) {
			srv.GET("/dca_posts", map[string]string{
				"Authorization": "Bearer " + signHMAC(t, alg, dcaSecret, claims),
			}).AssertStatus(http.StatusOK)
		})
	}
}

// MUST STILL WORK: widening the family must not have widened it to anything the
// header asks for. "none" is the classic forgery, and an asymmetric name on the
// HMAC path is the algorithm confusion the HS256-only guard was really about.
func TestDocContractAuth_OtherAlgorithmsAreStillRefused(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.JWTAuth(dcaSecret))
	})
	claims := map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}
	hdrFor := func(alg string) map[string]string {
		// Signed with HS256 but *labelled* something else: the label is what the
		// verifier must refuse, whatever the bytes are.
		tok := signHMAC(t, "HS256", dcaSecret, claims)
		parts := strings.SplitN(tok, ".", 2)
		hdr := base64.RawURLEncoding.EncodeToString(
			fmt.Appendf(nil, `{"alg":%q,"typ":"JWT"}`, alg))
		return map[string]string{"Authorization": "Bearer " + hdr + "." + parts[1]}
	}
	for _, alg := range []string{"none", "None", "RS256", "ES256", "HS1", "hs256", ""} {
		t.Run("alg_"+alg, func(t *testing.T) {
			resp := srv.GET("/dca_posts", hdrFor(alg))
			resp.AssertStatus(http.StatusUnauthorized)
			// The refusal should name what is accepted, since the old message
			// claimed HS256 was the only option even for a valid HS512.
			if !strings.Contains(string(resp.Body), "HS384") {
				t.Errorf("the refusal should name the accepted algorithms: %s", resp.Body)
			}
		})
	}
}

// MUST STILL WORK: a wider family must not weaken signature checking. A token
// labelled HS512 and signed with the wrong secret is still refused.
func TestDocContractAuth_WrongSecretStillFailsOnEveryAlgorithm(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(auth.JWTAuth(dcaSecret))
	})
	claims := map[string]any{"sub": "u1", "exp": time.Now().Add(time.Hour).Unix()}
	for _, alg := range []string{"HS256", "HS384", "HS512"} {
		t.Run(alg, func(t *testing.T) {
			srv.GET("/dca_posts", map[string]string{
				"Authorization": "Bearer " + signHMAC(t, alg, "a-completely-different-secret!!", claims),
			}).AssertStatus(http.StatusUnauthorized)
		})
	}
}

// ── (b) OwnerScope cannot fall through ────────────────────────────────────────

// With no principal the scope cannot be computed, so the request is refused
// rather than written. It used to be stored with the client's own value.
func TestDocContractAuth_OwnerScopeRefusesWithoutAPrincipal(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Service.Register(service.OwnerScope("user_id"),
			maniflex.ForOperation(maniflex.OpCreate))
	})

	srv.POST("/dca_posts", map[string]any{"title": "x", "user_id": "victim"}).
		AssertStatus(http.StatusUnauthorized)

	// And nothing was written, so the refusal is not cosmetic.
	if n := len(srv.GET("/dca_posts").DataList()); n != 0 {
		t.Errorf("%d record(s) stored despite the 401", n)
	}
}

// MUST STILL WORK: with a principal it stamps, and the client's value loses.
func TestDocContractAuth_OwnerScopeStillStampsTheCaller(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Auth = &maniflex.AuthInfo{UserID: "alice"}
			return next()
		})
		s.Pipeline.Service.Register(service.OwnerScope("user_id"),
			maniflex.ForOperation(maniflex.OpCreate))
	})

	created := srv.POST("/dca_posts", map[string]any{"title": "x", "user_id": "victim"})
	created.AssertStatus(http.StatusCreated)
	if got := created.Data()["user_id"]; got != "alice" {
		t.Errorf("user_id: got %v, want alice — the client's value must be replaced", got)
	}
}

// MUST STILL WORK (AUTH-5): an authenticated principal carrying no user id is
// refused too, rather than stamping "" and making every such caller one owner.
func TestDocContractAuth_OwnerScopeRefusesAnEmptyUserID(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Auth.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			ctx.Auth = &maniflex.AuthInfo{UserID: ""}
			return next()
		})
		s.Pipeline.Service.Register(service.OwnerScope("user_id"),
			maniflex.ForOperation(maniflex.OpCreate))
	})
	srv.POST("/dca_posts", map[string]any{"title": "x"}).
		AssertStatus(http.StatusUnauthorized)
}

// ── (c) the SetField examples ─────────────────────────────────────────────────

// The catalogue's example now stamps something that always exists, so copying
// it cannot panic. ctx.RequestID is set for every request.
func TestDocContractAuth_CatalogueSetFieldExampleIsSafeToCopy(t *testing.T) {
	t.Parallel()
	var seen string
	srv := dcaServer(t, func(s *maniflex.Server) {
		// Verbatim from service.md.
		s.Pipeline.Service.Register(
			service.SetField("title", func(ctx *maniflex.ServerContext) any {
				return ctx.RequestID
			}),
			maniflex.ForOperation(maniflex.OpCreate),
		)
		s.Pipeline.DB.Register(func(ctx *maniflex.ServerContext, next func() error) error {
			seen, _ = ctx.ParsedBody.Map()["title"].(string)
			return next()
		}, maniflex.ForOperation(maniflex.OpCreate))
	})

	srv.POST("/dca_posts", map[string]any{"title": "client value"}).
		AssertStatus(http.StatusCreated)
	if seen == "" || seen == "client value" {
		t.Errorf("the example must stamp the request id, got %q", seen)
	}
}

// The behaviour the old Go-doc example walked into, kept as documentation: a
// resolver returning nil CLEARS the field. That is deliberate (audit STEP-3) and
// must not be "fixed" into a skip — middleware needs a way to clear a value.
// Pinned so the semantics cannot drift silently, and so the docs' warning about
// it stays true.
func TestDocContractAuth_SetFieldReturningNilClearsTheColumn(t *testing.T) {
	t.Parallel()
	srv := dcaServer(t, func(s *maniflex.Server) {
		s.Pipeline.Service.Register(
			service.SetField("user_id", func(*maniflex.ServerContext) any { return nil }),
			maniflex.ForOperation(maniflex.OpUpdate),
		)
	})

	id := srv.MustID(srv.POST("/dca_posts", map[string]any{"title": "x", "user_id": "alice"}))
	if got := srv.GET("/dca_posts/" + id).Data()["user_id"]; got != "alice" {
		t.Fatalf("setup: user_id is %v, want alice", got)
	}

	srv.PATCH("/dca_posts/"+id, map[string]any{"title": "renamed"}).
		AssertStatus(http.StatusOK)
	if got := srv.GET("/dca_posts/" + id).Data()["user_id"]; got != nil {
		t.Errorf("user_id: got %v, want nil — returning nil clears the field, which "+
			"is why the docs steer identity stamps to OwnerScope", got)
	}
}
