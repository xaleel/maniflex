package e2e_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
	t.Setenv("ROT_KEY_INDEX", encodedTestKey(1))
	kp := &encryption.EnvKeyProvider{Prefix: "ROT_KEY", IndexKeyID: "index"}

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
	mac, _ := kp.HMAC(context.Background(), "index", []byte("ROTATE-VALUE"))

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

type rotationDoc struct {
	maniflex.BaseModel
	Secret string `json:"secret" db:"secret" mfx:"encrypted,key:new"`
}

type rotationUnique struct {
	maniflex.BaseModel
	NationalID string `json:"national_id" db:"national_id" mfx:"encrypted,unique,key:new"`
}

func encodedTestKey(fill byte) string {
	key := make([]byte, 32)
	for i := range key {
		key[i] = fill
	}
	return base64.StdEncoding.EncodeToString(key)
}

func sec05Provider(t *testing.T) *encryption.EnvKeyProvider {
	t.Helper()
	t.Setenv("SEC05_KEY_OLD", encodedTestKey(2))
	t.Setenv("SEC05_KEY_NEW", encodedTestKey(3))
	t.Setenv("SEC05_KEY_INDEX", encodedTestKey(4))
	return &encryption.EnvKeyProvider{Prefix: "SEC05_KEY", IndexKeyID: "index"}
}

func createEncryptedRotationRow(
	t *testing.T,
	db maniflex.DBAdapter,
	meta *maniflex.ModelMeta,
	kp *encryption.EnvKeyProvider,
	field, plaintext string,
	unique bool,
) string {
	t.Helper()
	env, err := kp.Encrypt(context.Background(), "old", []byte(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{field: "enc:" + base64.StdEncoding.EncodeToString(env)}
	if unique {
		mac, err := kp.HMAC(context.Background(), "index", []byte(plaintext))
		if err != nil {
			t.Fatal(err)
		}
		data[field+"_hmac"] = base64.StdEncoding.EncodeToString(mac)
	}
	rec, err := db.Create(context.Background(), meta, data)
	if err != nil {
		t.Fatal(err)
	}
	return maniflex.RecordToMap(meta, rec)["id"].(string)
}

func TestEncryption_RotationReportsEveryCorruptEnvelopeBeforeWriting(t *testing.T) {
	kp := sec05Provider(t)
	server := maniflex.New(maniflex.Config{KeyProvider: kp})
	server.MustRegister(rotationDoc{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatal(err)
	}
	meta, _ := server.Registry().Get("rotationDoc")

	validID := createEncryptedRotationRow(t, db, meta, kp, "secret", "valid", false)
	for _, stored := range []string{"enc:not-base64!", "enc:AA=="} {
		if _, err := db.Create(context.Background(), meta, map[string]any{"secret": stored}); err != nil {
			t.Fatal(err)
		}
	}

	report, rotErr := maniflex.RotateEncryptionKeyWithOptions(
		context.Background(), server, "rotationDoc", "old", "new",
		maniflex.EncryptionRotationOptions{PageSize: 1},
	)
	var rowErr *maniflex.EncryptionRotationError
	if !errors.As(rotErr, &rowErr) {
		t.Fatalf("error = %v, want *EncryptionRotationError", rotErr)
	}
	if got := len(report.Failures); got != 2 {
		t.Fatalf("failures = %d, want both corrupt rows reported: %#v", got, report.Failures)
	}
	if report.Rotated != 0 {
		t.Fatalf("preflight failure rotated %d rows, want 0", report.Rotated)
	}
	for _, failure := range report.Failures {
		if failure.RowID == "" || failure.Field != "secret" {
			t.Errorf("failure lacks row/field audit data: %#v", failure)
		}
	}

	rec, err := db.FindByID(context.Background(), meta, validID, &maniflex.QueryParams{})
	if err != nil {
		t.Fatal(err)
	}
	raw := maniflex.RecordToMap(meta, rec)["secret"].(string)
	env, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "enc:"))
	if err != nil {
		t.Fatal(err)
	}
	if keyID, _ := kp.KeyIDOf(env); keyID != "old" {
		t.Errorf("valid row changed despite failed preflight: key = %q, want old", keyID)
	}
}

func TestEncryption_RotationRejectsLegacyBlindIndexOnAlreadyRotatedRow(t *testing.T) {
	kp := sec05Provider(t)
	server := maniflex.New(maniflex.Config{KeyProvider: kp})
	server.MustRegister(rotationUnique{})

	db, err := sqlite.Open(":memory:", server.Registry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server.SetDB(db)
	if err := db.AutoMigrate(context.Background(), server.Registry()); err != nil {
		t.Fatal(err)
	}
	meta, _ := server.Registry().Get("rotationUnique")

	plaintext := []byte("legacy-index")
	env, err := kp.Encrypt(context.Background(), "new", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	legacyMAC, err := kp.HMAC(context.Background(), "new", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Create(context.Background(), meta, map[string]any{
		"national_id":      "enc:" + base64.StdEncoding.EncodeToString(env),
		"national_id_hmac": base64.StdEncoding.EncodeToString(legacyMAC),
	}); err != nil {
		t.Fatal(err)
	}

	report, rotErr := maniflex.RotateEncryptionKeyWithOptions(
		context.Background(), server, "rotationUnique", "old", "new",
		maniflex.EncryptionRotationOptions{},
	)
	var rowErr *maniflex.EncryptionRotationError
	if !errors.As(rotErr, &rowErr) {
		t.Fatalf("error = %v, want *EncryptionRotationError", rotErr)
	}
	if len(report.Failures) != 1 || report.Failures[0].Stage != "blind-index" {
		t.Fatalf("failures = %#v, want the already-rotated legacy digest", report.Failures)
	}
	if report.Complete || report.Rotated != 0 {
		t.Fatalf("unsafe report = %#v", report)
	}
}

type cancelAfterUpdateAdapter struct {
	maniflex.DBAdapter
	cancel context.CancelFunc
	once   sync.Once
}

func (a *cancelAfterUpdateAdapter) Update(
	ctx context.Context,
	model *maniflex.ModelMeta,
	id string,
	record any,
	present map[string]struct{},
) (any, error) {
	updated, err := a.DBAdapter.Update(ctx, model, id, record, present)
	if err == nil {
		a.once.Do(a.cancel)
	}
	return updated, err
}

func TestEncryption_RotationUsesModelAdapterAndResumesAfterInterruption(t *testing.T) {
	kp := sec05Provider(t)
	inner, err := sqlite.Open(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	adapter := &cancelAfterUpdateAdapter{DBAdapter: inner, cancel: cancel}
	server := maniflex.New(maniflex.Config{KeyProvider: kp}) // no global DB
	server.MustRegister(rotationDoc{}, maniflex.ModelConfig{Adapter: adapter})
	if err := server.MigrateOnly(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta, _ := server.Registry().Get("rotationDoc")
	for _, plaintext := range []string{"one", "two", "three"} {
		createEncryptedRotationRow(t, inner, meta, kp, "secret", plaintext, false)
	}

	first, rotErr := maniflex.RotateEncryptionKeyWithOptions(
		ctx, server, "rotationDoc", "old", "new",
		maniflex.EncryptionRotationOptions{PageSize: 1},
	)
	if !errors.Is(rotErr, context.Canceled) {
		t.Fatalf("first rotation error = %v, want context.Canceled", rotErr)
	}
	if first.Rotated != 1 || first.LastID == "" || first.Complete {
		t.Fatalf("interrupted report = %#v, want one row and a resume cursor", first)
	}

	resumed, err := maniflex.RotateEncryptionKeyWithOptions(
		context.Background(), server, "rotationDoc", "old", "new",
		maniflex.EncryptionRotationOptions{AfterID: first.LastID, PageSize: 1},
	)
	if err != nil {
		t.Fatalf("resume rotation: %v", err)
	}
	if !resumed.Complete || resumed.Rotated != 2 {
		t.Fatalf("resumed report = %#v, want two remaining rows complete", resumed)
	}

	rows, _, err := inner.FindMany(context.Background(), meta, &maniflex.QueryParams{
		Page: 1, Limit: 10, Sorts: []maniflex.SortExpr{{DBName: "id", Direction: maniflex.SortAsc}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		raw := maniflex.RecordToMap(meta, row)["secret"].(string)
		env, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, "enc:"))
		if err != nil {
			t.Fatal(err)
		}
		if keyID, _ := kp.KeyIDOf(env); keyID != "new" {
			t.Errorf("row remained on key %q after resume", keyID)
		}
	}
}

type pausedUpdateAdapter struct {
	maniflex.DBAdapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *pausedUpdateAdapter) Update(
	ctx context.Context,
	model *maniflex.ModelMeta,
	id string,
	record any,
	present map[string]struct{},
) (any, error) {
	a.once.Do(func() { close(a.entered) })
	select {
	case <-a.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.DBAdapter.Update(ctx, model, id, record, present)
}

func TestEncryption_ConcurrentDuplicateCannotBypassRotation(t *testing.T) {
	kp := sec05Provider(t)
	var inner maniflex.DBAdapter
	gated := &pausedUpdateAdapter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-gated.release:
		default:
			close(gated.release)
		}
	}()
	srv := testutil.NewServer(t, testutil.Options{
		Models:      []any{rotationUnique{}},
		KeyProvider: kp,
		DBAdapter: func(reg maniflex.RegistryAccessor) (maniflex.DBAdapter, error) {
			var err error
			inner, err = sqlite.Open(":memory:", reg)
			if err != nil {
				return nil, err
			}
			gated.DBAdapter = inner
			return gated, nil
		},
	})
	meta, _ := srv.ManiflexServer().Registry().Get("rotationUnique")
	createEncryptedRotationRow(t, inner, meta, kp, "national_id", "DUPLICATE", true)

	type result struct {
		report maniflex.EncryptionRotationReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := maniflex.RotateEncryptionKeyWithOptions(
			context.Background(), srv.ManiflexServer(), "rotationUnique", "old", "new",
			maniflex.EncryptionRotationOptions{PageSize: 1},
		)
		done <- result{report: report, err: err}
	}()

	select {
	case <-gated.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rotation did not reach the paused mixed-key state")
	}

	duplicate := srv.POST("/rotation_uniques", map[string]any{"national_id": "DUPLICATE"})
	duplicate.AssertStatus(http.StatusConflict)
	close(gated.release)

	rotated := <-done
	if rotated.err != nil || !rotated.report.Complete || rotated.report.Rotated != 1 {
		t.Fatalf("rotation result = %#v, error = %v", rotated.report, rotated.err)
	}
}
