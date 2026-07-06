package e2e_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/db/sqlite"
	"github.com/xaleel/maniflex/pkg/encryption"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// ── Test model ────────────────────────────────────────────────────────────────

type Patient struct {
	maniflex.BaseModel
	NationalID string `json:"national_id" db:"national_id" mfx:"encrypted,unique"`
	DOB        string `json:"dob"         db:"dob"         mfx:"encrypted"`
	Name       string `json:"name"        db:"name"`
}

// testEncKey is a fixed 32-byte AES-256 key, base64-encoded. Tests only.
const testEncKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func patientServer(t *testing.T) *testutil.Server {
	t.Helper()
	t.Setenv("TESTENC_KEY_DEFAULT", testEncKey)
	return testutil.NewServer(t, testutil.Options{
		Models:      []any{Patient{}},
		KeyProvider: &encryption.EnvKeyProvider{Prefix: "TESTENC_KEY"},
	})
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestEncryption_RoundTrip(t *testing.T) {
	srv := patientServer(t)

	resp := srv.POST("/patients", map[string]any{
		"national_id": "123-45-6789",
		"dob":         "1990-01-15",
		"name":        "Alice",
	})
	resp.AssertStatus(http.StatusCreated)
	data := resp.Data()

	if got := data["national_id"]; got != "123-45-6789" {
		t.Errorf("POST national_id: got %v, want 123-45-6789", got)
	}
	if got := data["dob"]; got != "1990-01-15" {
		t.Errorf("POST dob: got %v, want 1990-01-15", got)
	}

	id := data["id"].(string)
	gd := srv.GET("/patients/" + id).AssertStatus(http.StatusOK).Data()
	if got := gd["national_id"]; got != "123-45-6789" {
		t.Errorf("GET national_id: got %v, want 123-45-6789", got)
	}
	if got := gd["dob"]; got != "1990-01-15" {
		t.Errorf("GET dob: got %v, want 1990-01-15", got)
	}
}

func TestEncryption_ListDecrypts(t *testing.T) {
	srv := patientServer(t)

	srv.MustID(srv.POST("/patients", map[string]any{
		"national_id": "LIST-ID-1",
		"dob":         "1985-03-20",
		"name":        "Bob",
	}))

	items := srv.GET("/patients").AssertStatus(http.StatusOK).DataList()
	if len(items) == 0 {
		t.Fatal("expected at least one patient in list")
	}
	first := items[0].(map[string]any)
	if got := first["national_id"]; got != "LIST-ID-1" {
		t.Errorf("list national_id: got %v, want LIST-ID-1", got)
	}
}

func TestEncryption_UpdateRoundTrip(t *testing.T) {
	srv := patientServer(t)

	id := srv.MustID(srv.POST("/patients", map[string]any{
		"national_id": "OLD-ID",
		"dob":         "1970-06-15",
		"name":        "Charlie",
	}))

	srv.PATCH("/patients/"+id, map[string]any{"national_id": "NEW-ID"}).
		AssertStatus(http.StatusOK)

	gd := srv.GET("/patients/" + id).Data()
	if got := gd["national_id"]; got != "NEW-ID" {
		t.Errorf("GET after PATCH national_id: got %v, want NEW-ID", got)
	}
}

func TestEncryption_HMACEnforcesUniqueness(t *testing.T) {
	srv := patientServer(t)

	srv.MustID(srv.POST("/patients", map[string]any{
		"national_id": "DUPLICATE-NID",
		"dob":         "1980-01-01",
		"name":        "Original",
	}))

	dup := srv.POST("/patients", map[string]any{
		"national_id": "DUPLICATE-NID",
		"dob":         "1980-02-02",
		"name":        "Duplicate",
	})
	dup.AssertStatus(http.StatusConflict)
	if dup.ErrorCode() != "CONFLICT" {
		t.Errorf("error code: got %q, want CONFLICT", dup.ErrorCode())
	}
}

func TestEncryption_FilterRejected(t *testing.T) {
	srv := patientServer(t)

	resp := srv.GET("/patients?filter=national_id:eq:anything")
	resp.AssertStatus(http.StatusBadRequest)
	resp.AssertJSON(func(body map[string]any) {
		errObj, _ := body["error"].(map[string]any)
		msg, _ := errObj["message"].(string)
		if !strings.Contains(msg, "ENCRYPTED_FIELD_NOT_FILTERABLE") {
			t.Errorf("expected ENCRYPTED_FIELD_NOT_FILTERABLE in error message, got: %q", msg)
		}
	})
}

func TestEncryption_HMACColumnsNotExposedInResponse(t *testing.T) {
	srv := patientServer(t)

	id := srv.MustID(srv.POST("/patients", map[string]any{
		"national_id": "HMAC-LEAK-CHECK",
		"dob":         "1999-12-31",
		"name":        "Hmac Test",
	}))

	for key := range srv.GET("/patients/" + id).Data() {
		if strings.HasSuffix(key, "_hmac") {
			t.Errorf("response contains HMAC column %q — must never be exposed", key)
		}
	}
}

func TestEncryption_NoProviderRejectsWrite(t *testing.T) {
	srv := testutil.NewServer(t, testutil.Options{
		Models: []any{Patient{}},
		// KeyProvider intentionally omitted
	})

	resp := srv.POST("/patients", map[string]any{
		"national_id": "SHOULD-FAIL",
		"dob":         "2000-01-01",
		"name":        "NoKey",
	})
	resp.AssertStatus(http.StatusInternalServerError)
	if resp.ErrorCode() != "ENCRYPTION_NOT_CONFIGURED" {
		t.Errorf("error code: got %q, want ENCRYPTION_NOT_CONFIGURED", resp.ErrorCode())
	}
}

func TestEncryption_RotateEncryptionKey(t *testing.T) {
	t.Setenv("ROT_KEY_DEFAULT", testEncKey)
	kp := &encryption.EnvKeyProvider{Prefix: "ROT_KEY"}

	server := maniflex.New(maniflex.Config{
		PathPrefix:  "/api",
		KeyProvider: kp,
	})
	server.MustRegister(Patient{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatal(err)
	}
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatal(err)
	}

	meta, _ := server.Registry().Get("Patient")

	// Pre-encrypt a row directly so we control the keyID in the envelope.
	envBytes, err := kp.Encrypt(context.Background(), "default", []byte("ROTATE-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	dobEnv, _ := kp.Encrypt(context.Background(), "default", []byte("1990-01-01"))
	mac, _ := kp.HMAC(context.Background(), "default", []byte("ROTATE-VALUE"))

	_, err = db.Create(context.Background(), meta, map[string]any{
		"national_id":      "enc:" + base64.StdEncoding.EncodeToString(envBytes),
		"national_id_hmac": base64.StdEncoding.EncodeToString(mac),
		"dob":              "enc:" + base64.StdEncoding.EncodeToString(dobEnv),
		"name":             "Rotate Patient",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Rotate "default" → "default" (same key, same keyID — verifies the
	// re-encrypt path runs without error and updates the row).
	rotated, rotErr := maniflex.RotateEncryptionKey(context.Background(), server, "Patient", "default", "default")
	if rotErr != nil {
		t.Fatalf("RotateEncryptionKey: %v", rotErr)
	}
	if rotated != 1 {
		t.Errorf("rotated count: got %d, want 1", rotated)
	}

	// After rotation the row must still be readable through the full pipeline.
	rows, _, err := db.FindMany(context.Background(), meta, &maniflex.QueryParams{Page: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected 1 row after rotation")
	}
	raw := maniflex.RecordToMap(meta, rows[0])["national_id"].(string)
	if !strings.HasPrefix(raw, "enc:") {
		t.Errorf("after rotation national_id should still be enc:..., got %q", raw)
	}
}
