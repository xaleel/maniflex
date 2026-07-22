package e2e

// Audit JB-14: a job whose payload was encrypted under the previous key must
// still run after the key is rotated. The payload column now holds maniflex's
// self-describing envelope, which carries the key id, so the provider decrypts it
// with the key it was written under rather than the current one.
//
//	go test ./e2e/ -run TestJobsSQLKeyRotation

import (
	"context"
	"crypto/rand"
	stdsql "database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xaleel/maniflex/jobs"
	jobssql "github.com/xaleel/maniflex/jobs/sql"
	"github.com/xaleel/maniflex/pkg/encryption"
)

func rotationProvider(t *testing.T, prefix string, keyIDs ...string) *encryption.EnvKeyProvider {
	t.Helper()
	for _, id := range keyIDs {
		var k [32]byte
		if _, err := rand.Read(k[:]); err != nil {
			t.Fatalf("key material: %v", err)
		}
		t.Setenv(prefix+"_"+strings.ToUpper(id), base64.StdEncoding.EncodeToString(k[:]))
	}
	return &encryption.EnvKeyProvider{Prefix: prefix}
}

func TestJobsSQLKeyRotation_JobEnqueuedUnderOldKeyStillRuns(t *testing.T) {
	db := rawJobsDB(t)
	const table = "key_rotation"
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, jobsDriver(), jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	kp := rotationProvider(t, "MFXROT", "k1", "k2")
	payload := json.RawMessage(`{"invoice":"INV-7"}`)

	// Enqueued while the queue was writing under k1.
	old := jobssql.New(db, jobssql.WithTableName(table), jobssql.WithKeyProvider(kp, "k1"))
	id, err := old.Enqueue(ctx, jobs.Job{Type: "rotate.test", Payload: payload})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// At rest it is an envelope, not the payload.
	var stored string
	if err := db.QueryRow(ph(`SELECT payload FROM `+table+` WHERE id=?`), id).Scan(&stored); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") || strings.Contains(stored, "INV-7") {
		t.Errorf("stored payload = %q, want an opaque enc: envelope", stored)
	}

	// The key has since rotated: the queue now writes under k2, and the provider
	// still resolves k1 for what is already in flight.
	rotated := jobssql.New(db, jobssql.WithTableName(table), jobssql.WithKeyProvider(kp, "k2"))
	claimed, err := rotated.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("got %d jobs after rotation, want 1 — a job encrypted under the "+
			"previous key was quarantined instead of run", len(claimed))
	}
	if string(claimed[0].Payload) != string(payload) {
		t.Errorf("payload = %s, want %s", claimed[0].Payload, payload)
	}
}

// The other half of JB-14: a queue that has lost the means to decrypt must not
// hand the stored ciphertext to a handler as its payload. It is quarantined.
func TestJobsSQLKeyRotation_UndecryptablePayloadIsQuarantinedNotDelivered(t *testing.T) {
	db := rawJobsDB(t)
	const table = "key_lost"
	ctx := context.Background()
	if err := jobssql.Migrate(ctx, db, jobsDriver(), jobssql.WithTableName(table)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	kp := rotationProvider(t, "MFXHAVE", "k1")

	enc := jobssql.New(db, jobssql.WithTableName(table), jobssql.WithKeyProvider(kp, "k1"))
	id, err := enc.Enqueue(ctx, jobs.Job{Type: "rotate.test", Payload: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The option was dropped (or the key retired): nothing can decrypt this row.
	plain := jobssql.New(db, jobssql.WithTableName(table))
	claimed, err := plain.Dequeue(ctx, 10)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	for _, j := range claimed {
		if strings.HasPrefix(string(j.Payload), "enc:") {
			t.Errorf("ciphertext was delivered to a handler as the payload: %s", j.Payload)
		}
	}
	if len(claimed) != 0 {
		t.Errorf("got %d jobs, want 0 — the undecryptable row must not be delivered", len(claimed))
	}

	var status string
	var lastErr stdsql.NullString
	if err := db.QueryRow(ph(`SELECT status, last_error FROM `+table+` WHERE id=?`), id).
		Scan(&status, &lastErr); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "dead" {
		t.Errorf("undecryptable row status = %q, want \"dead\" (quarantined)", status)
	}
	if !lastErr.Valid || !strings.Contains(lastErr.String, "KeyProvider") {
		t.Errorf("last_error = %q, want it to name the missing KeyProvider", lastErr.String)
	}
}
