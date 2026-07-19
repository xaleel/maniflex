package e2e

// Audit MS-10: the "_through" payload on a many-to-many include was copied
// verbatim from the junction row — every column except the two FKs and id — so
// the junction model's own tags were the one thing it did not consult. A
// mfx:"hidden" or mfx:"writeonly" column surfaced raw on every include.
//
// Response-step filtering does not catch it: toJSONMap treats underscore-
// prefixed keys as framework-reserved and passes them through untouched. So the
// payload has to be built correctly at the source.
//
//	go test ./tests/e2e/... -run TestThrough

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

type ThProduct struct {
	maniflex.BaseModel
	Name string  `json:"name" db:"name"`
	Tags []ThTag `json:"tags" mfx:"through:ThProductTag"`
}

type ThTag struct {
	maniflex.BaseModel
	Label string `json:"label" db:"label"`
}

// ThProductTag is the junction. It carries one column of each kind whose tag the
// payload has to honour, plus an ordinary one that must survive.
type ThProductTag struct {
	maniflex.BaseModel
	ThProductID string `json:"th_product_id" db:"th_product_id" mfx:"filterable,relation"`
	ThTagID     string `json:"th_tag_id"     db:"th_tag_id"     mfx:"filterable,relation"`
	Note        string `json:"note"          db:"note"`
	InvitedBy   string `json:"invited_by"    db:"invited_by"   mfx:"hidden"`
	SecretNote  string `json:"secret_note"   db:"secret_note"  mfx:"writeonly"`
	Cipher      string `json:"cipher"        db:"cipher"       mfx:"encrypted"`
}

// thEncKey is a fixed 32-byte AES-256 key, base64-encoded. Tests only.
// (encryption_test.go's testEncKey lives in package e2e_test, not e2e.)
const thEncKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func thServer(t *testing.T) *testutil.Server {
	t.Helper()
	t.Setenv("TESTENC_KEY_DEFAULT", thEncKey)
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{ThProduct{}, ThTag{}, ThProductTag{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
	})
}

// thLinked creates a product, a tag and the junction row between them, and
// returns the product id plus the raw body of an include of its tags.
func thLinked(t *testing.T, srv *testutil.Server) (string, string) {
	t.Helper()
	pid := srv.MustID(srv.POST("/th_products", map[string]any{"name": "widget"}))
	tid := srv.MustID(srv.POST("/th_tags", map[string]any{"label": "blue"}))
	srv.POST("/th_product_tags", map[string]any{
		"th_product_id": pid, "th_tag_id": tid,
		"note":        "PUBLIC-NOTE",
		"secret_note": "WRITEONLY-LEAK",
		"cipher":      "ENCRYPTED-LEAK",
	}).AssertStatus(http.StatusCreated)

	return pid, string(srv.GET("/th_products/" + pid + "?include=tags").Body)
}

// The headline: a tag that hides a column from responses hides it here too.
func TestThrough_HonoursJunctionTags(t *testing.T) {
	srv := thServer(t)
	_, body := thLinked(t, srv)

	// Values must not appear...
	for _, leak := range []string{"WRITEONLY-LEAK", "ENCRYPTED-LEAK"} {
		if strings.Contains(body, leak) {
			t.Errorf("_through leaked %q: %s", leak, body)
		}
	}
	// ...and neither must the keys, which disclose the column's existence even
	// when the value happens to be empty.
	for _, key := range []string{"invited_by", "secret_note", "cipher"} {
		if strings.Contains(body, key) {
			t.Errorf("_through exposes the %q column: %s", key, body)
		}
	}
	// The ciphertext envelope must not leak either: it is no use to a client and
	// discloses the encryption metadata.
	if strings.Contains(body, "enc:") {
		t.Errorf("_through carries ciphertext: %s", body)
	}
}

// The payload must still carry what it is for. Without this the fix could drop
// _through entirely and pass every assertion above.
func TestThrough_OrdinaryPayloadSurvives(t *testing.T) {
	srv := thServer(t)
	_, body := thLinked(t, srv)

	if !strings.Contains(body, `"_through"`) {
		t.Fatalf("the _through object is missing entirely: %s", body)
	}
	if !strings.Contains(body, "PUBLIC-NOTE") {
		t.Errorf("an ordinary junction column must still be exposed: %s", body)
	}
	// And the related row itself is unaffected.
	if !strings.Contains(body, `"label":"blue"`) {
		t.Errorf("the included row lost its own fields: %s", body)
	}
}

// The FKs and the junction's id are excluded, as they always were: they say
// nothing the response does not already carry.
func TestThrough_ExcludesKeysAndFKs(t *testing.T) {
	srv := thServer(t)
	pid, body := thLinked(t, srv)

	start := strings.Index(body, `"_through"`)
	if start < 0 {
		t.Fatalf("no _through in %s", body)
	}
	end := strings.Index(body[start:], "}")
	through := body[start : start+end]

	for _, key := range []string{"th_product_id", "th_tag_id"} {
		if strings.Contains(through, key) {
			t.Errorf("_through should not repeat the foreign key %q: %s", key, through)
		}
	}
	if strings.Contains(through, pid) {
		t.Errorf("_through should not repeat the parent id: %s", through)
	}
}
