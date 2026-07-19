package e2e_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// Audit MS-3: the versioning snapshot is built from the *decrypted* post-image,
// so every create/update/delete on a Versioned model wrote the plaintext of
// encrypted columns — and of hidden/write-only fields such as password hashes —
// into {model}_history.snapshot as cleartext JSON. computeDiff already excluded
// those columns; chooseSnapshot did not, silently defeating at-rest encryption
// for the history table.
//
//	go test ./tests/e2e/... -run TestHistoryRedaction

// HistSecret is versioned with snapshots on (the default) and carries one field
// of each kind the history table must not record in the clear.
type HistSecret struct {
	maniflex.BaseModel `mfx:"versioned"`
	Title              string `json:"title"    db:"title"`
	SSN                string `json:"ssn"      db:"ssn"      mfx:"encrypted"`
	Password           string `json:"password" db:"password" mfx:"writeonly"`
	Internal           string `json:"internal" db:"internal" mfx:"hidden,writeonly"`
}

// The secret values, distinct so a leak names which field leaked.
const (
	secretSSN      = "SSN-PLAINTEXT-111"
	secretPassword = "PASSWORD-PLAINTEXT-222"
	secretInternal = "INTERNAL-PLAINTEXT-333"
	secretNewSSN   = "SSN-PLAINTEXT-444"
)

func histSecretServer(t *testing.T) *testutil.Server {
	t.Helper()
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{HistSecret{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
	})
}

// snapshotsFor returns the raw snapshot column of every history row for id, in
// version order. Reading the stored string rather than a parsed projection is
// the point: a leak is a leak whatever shape it is stored in.
func snapshotsFor(t *testing.T, srv *testutil.Server, id string) []string {
	t.Helper()
	items := srv.GET(fmt.Sprintf(
		"/hist_secret_history?filter=record_id:eq:%s&sort=version", id)).DataList()
	out := make([]string, 0, len(items))
	for _, it := range items {
		row, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("history row is %T, want map", it)
		}
		snap, ok := row["snapshot"].(string)
		if !ok || snap == "" {
			t.Fatalf("history version %v has no snapshot — the test would prove "+
				"nothing about redaction; snapshots must be on for this model", row["version"])
		}
		out = append(out, snap)
	}
	return out
}

// assertSnapCount guards the index reads below.
func assertSnapCount(t *testing.T, snaps []string, want int) {
	t.Helper()
	if len(snaps) != want {
		t.Fatalf("history rows: got %d, want %d", len(snaps), want)
	}
}

// assertNoPlaintext fails naming both the field and the operation that leaked.
func assertNoPlaintext(t *testing.T, op, snapshot string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(snapshot, s) {
			t.Errorf("%s snapshot contains %q in the clear: %s", op, s, snapshot)
		}
	}
}

func createHistSecret(t *testing.T, srv *testutil.Server) string {
	t.Helper()
	return srv.MustID(srv.POST("/hist_secrets", map[string]any{
		"title":    "Report",
		"ssn":      secretSSN,
		"password": secretPassword,
		"internal": secretInternal,
	}))
}

// Create writes the post-image. It must not carry the protected columns.
func TestHistoryRedaction_CreateSnapshotOmitsProtectedFields(t *testing.T) {
	srv := histSecretServer(t)
	id := createHistSecret(t, srv)

	snaps := snapshotsFor(t, srv, id)
	assertSnapCount(t, snaps, 1)
	assertNoPlaintext(t, "create", snaps[0], secretSSN, secretPassword, secretInternal)
}

// Update writes the post-image too, including the newly written secret.
func TestHistoryRedaction_UpdateSnapshotOmitsProtectedFields(t *testing.T) {
	srv := histSecretServer(t)
	id := createHistSecret(t, srv)

	srv.PATCH("/hist_secrets/"+id, map[string]any{
		"title": "Revised",
		"ssn":   secretNewSSN,
	}).AssertStatus(http.StatusOK)

	snaps := snapshotsFor(t, srv, id)
	assertSnapCount(t, snaps, 2)
	assertNoPlaintext(t, "update", snaps[1],
		secretSSN, secretNewSSN, secretPassword, secretInternal)
}

// Delete snapshots the pre-image, which comes from a different code path (a
// FindByID that decrypts) — so it needs its own assertion, not an inference.
func TestHistoryRedaction_DeleteSnapshotOmitsProtectedFields(t *testing.T) {
	srv := histSecretServer(t)
	id := createHistSecret(t, srv)

	srv.DELETE("/hist_secrets/" + id).AssertStatus(http.StatusNoContent)

	snaps := snapshotsFor(t, srv, id)
	assertSnapCount(t, snaps, 2)
	assertNoPlaintext(t, "delete", snaps[1], secretSSN, secretPassword, secretInternal)
}

// Redaction must not gut the snapshot: the non-protected columns are what the
// feature is for, and dropping them would pass every test above vacuously.
func TestHistoryRedaction_SnapshotKeepsOrdinaryFields(t *testing.T) {
	srv := histSecretServer(t)
	id := createHistSecret(t, srv)

	snaps := snapshotsFor(t, srv, id)
	for _, want := range []string{`"title"`, `"Report"`, `"id"`, id} {
		if !strings.Contains(snaps[0], want) {
			t.Errorf("snapshot lost %s — redaction must drop protected columns, "+
				"not the record: %s", want, snaps[0])
		}
	}
}

// The HMAC companion column is derived from the plaintext and is an offline
// dictionary attack away from it, so it is excluded alongside its source.
func TestHistoryRedaction_SnapshotOmitsHMACColumn(t *testing.T) {
	srv := histSecretServer(t)
	id := createHistSecret(t, srv)

	snaps := snapshotsFor(t, srv, id)
	for _, banned := range []string{"ssn_hmac", `"ssn"`, `"password"`, `"internal"`} {
		if strings.Contains(snaps[0], banned) {
			t.Errorf("snapshot contains protected column %s: %s", banned, snaps[0])
		}
	}
}
