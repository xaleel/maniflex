package maniflex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// KeyProvider manages encryption keys for mfx:"encrypted" struct fields.
//
// Values are stored in the database as the string  "enc:<base64(envelope)>".
// The "enc:" prefix lets the framework distinguish already-encrypted values from
// unencrypted legacy data, so tables can be migrated incrementally.
//
// Use pkg/encryption.EnvKeyProvider for environment-variable-backed keys, or
// pkg/encryption.VaultKeyProvider for HashiCorp Vault Transit.
type KeyProvider interface {
	// Encrypt encrypts plaintext under keyID and returns a self-describing
	// binary envelope that embeds the keyID. The keyID is only needed at write
	// time; Decrypt reads it from the envelope automatically.
	Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error)

	// Decrypt decrypts an envelope produced by Encrypt. The keyID is extracted
	// from the envelope — callers do not need to supply it.
	Decrypt(ctx context.Context, envelope []byte) ([]byte, error)

	// KeyIDOf extracts the keyID from an envelope without decrypting. Useful
	// for audit logging and key rotation checks.
	KeyIDOf(envelope []byte) (string, error)

	// HMAC returns a deterministic keyed digest of data under keyID. Used to
	// enforce UNIQUE constraints on encrypted fields: a companion
	// {field}_hmac TEXT UNIQUE column stores this digest so uniqueness can be
	// checked without exposing or comparing ciphertexts.
	HMAC(ctx context.Context, keyID string, data []byte) ([]byte, error)
}

// BlindIndexKeyProvider is an optional KeyProvider capability that keeps
// encrypted-field uniqueness digests under a key independent of field
// encryption keys. RotateEncryptionKey requires a non-empty blind-index key
// for models with encrypted+unique fields.
type BlindIndexKeyProvider interface {
	BlindIndexKeyID() string
}

const (
	encStoragePrefix     = "enc:"
	defaultEncryptionKey = "default"
)

func blindIndexKeyID(kp KeyProvider, encryptionKeyID string) (keyID string, stable bool) {
	if p, ok := kp.(BlindIndexKeyProvider); ok {
		if keyID := strings.TrimSpace(p.BlindIndexKeyID()); keyID != "" {
			return keyID, true
		}
	}
	// Preserve legacy writes for applications which have not configured a
	// dedicated index key. Rotation refuses this fallback because its digest
	// changes with the encryption key.
	return encryptionKeyID, false
}

// encryptFields encrypts all mfx:"encrypted" fields in the DB-keyed data map.
// For encrypted+unique fields it also writes a keyed HMAC into {field}_hmac.
// Returns an error if any field cannot be encrypted (caller should abort 500).
func encryptFields(ctx context.Context, kp KeyProvider, model *ModelMeta, data map[string]any) error {
	for _, f := range model.EncryptedFields() {
		dbName := f.Tags.DBName
		val, ok := data[dbName]
		if !ok || val == nil {
			continue
		}
		plaintext := fmt.Sprint(val)
		if plaintext == "" {
			// The column is being set to "", so its HMAC companion must be
			// cleared with it. Skipping the whole field left the digest of the
			// *previous* value behind (audit MS-L6), and that column is what the
			// uniqueness check consults: a stale digest keeps blocking the value
			// this record no longer holds, and lets the record itself pass a
			// check it should fail. Nothing to encrypt, so only the companion is
			// written.
			if f.Tags.Unique {
				data[dbName+"_hmac"] = ""
			}
			continue
		}

		keyID := f.Tags.EncryptionKey
		if keyID == "" {
			keyID = defaultEncryptionKey
		}

		envelope, err := kp.Encrypt(ctx, keyID, []byte(plaintext))
		if err != nil {
			return fmt.Errorf("encrypt field %q: %w", dbName, err)
		}
		data[dbName] = encStoragePrefix + base64.StdEncoding.EncodeToString(envelope)

		if f.Tags.Unique {
			indexKeyID, _ := blindIndexKeyID(kp, keyID)
			mac, err := kp.HMAC(ctx, indexKeyID, []byte(plaintext))
			if err != nil {
				return fmt.Errorf("hmac field %q: %w", dbName, err)
			}
			data[dbName+"_hmac"] = base64.StdEncoding.EncodeToString(mac)
		}
	}
	return nil
}

// decryptFields decrypts all mfx:"encrypted" fields in the DB-keyed data map in
// place. HMAC columns are always stripped. Values that are not encrypted yet
// (no "enc:" prefix) are left unchanged, enabling gradual migration.
// Returns an error if a properly-formatted envelope cannot be decrypted.
func decryptFields(ctx context.Context, kp KeyProvider, model *ModelMeta, data map[string]any) error {
	for _, f := range model.EncryptedFields() {
		dbName := f.Tags.DBName
		delete(data, dbName+"_hmac") // never expose HMAC columns via API

		val, ok := data[dbName]
		if !ok || val == nil {
			continue
		}
		stored, ok := val.(string)
		if !ok || stored == "" {
			continue
		}

		// Legacy value (not yet encrypted): leave as-is.
		if !strings.HasPrefix(stored, encStoragePrefix) {
			continue
		}

		b64 := strings.TrimPrefix(stored, encStoragePrefix)
		envelope, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("decrypt field %q: invalid base64 envelope: %w", dbName, err)
		}

		plaintext, err := kp.Decrypt(ctx, envelope)
		if err != nil {
			return fmt.Errorf("decrypt field %q: %w", dbName, err)
		}
		data[dbName] = string(plaintext)
	}
	return nil
}

// encryptForWrite encrypts a write map's mfx:"encrypted" fields when the model
// declares any. It is the shared entry point for the non-pipeline write paths
// (typed maniflex.Create/Update and ctx.GetModel), mirroring what the HTTP DB
// step does. Returns an error if encrypted fields are present but no KeyProvider
// is configured. No-op for models without encrypted fields.
func encryptForWrite(ctx context.Context, kp KeyProvider, model *ModelMeta, data map[string]any) error {
	if !model.HasEncryptedFields() {
		return nil
	}
	if kp == nil {
		return fmt.Errorf("maniflex: model %q has mfx:\"encrypted\" fields but no KeyProvider is configured", model.Name)
	}
	return encryptFields(ctx, kp, model, data)
}

// decryptForRead decrypts a read map's mfx:"encrypted" fields in place. With no
// KeyProvider it still strips the {field}_hmac companion columns so they never
// surface. No-op for models without encrypted fields.
func decryptForRead(ctx context.Context, kp KeyProvider, model *ModelMeta, data map[string]any) error {
	if !model.HasEncryptedFields() {
		return nil
	}
	if kp == nil {
		stripHMACColumns(model, data)
		return nil
	}
	return decryptFields(ctx, kp, model, data)
}

// stripHMACColumns removes {field}_hmac keys from data for all encrypted fields
// that carry a unique constraint. Called on reads when no KeyProvider is set so
// HMAC columns are never surfaced through the API.
func stripHMACColumns(model *ModelMeta, data map[string]any) {
	for _, f := range model.EncryptedFields() {
		if f.Tags.Unique {
			delete(data, f.Tags.DBName+"_hmac")
		}
	}
}

// EncryptionRotationOptions controls a resumable encryption-key rotation.
type EncryptionRotationOptions struct {
	// AfterID resumes strictly after this record ID. Use LastID after an
	// interruption or adapter error. Reported row failures must instead be
	// repaired and preflighted again from the beginning.
	AfterID string

	// PageSize controls keyset page size. Zero uses 100.
	PageSize int
}

// EncryptionRotationFailure identifies a row which could not be safely
// inspected or transformed. Rotation never silently skips one of these.
type EncryptionRotationFailure struct {
	RowID string
	Field string
	Stage string
	Err   error
}

func (f EncryptionRotationFailure) Error() string {
	return fmt.Sprintf("row %q field %q (%s): %v", f.RowID, f.Field, f.Stage, f.Err)
}

// EncryptionRotationReport is the auditable result of a rotation attempt.
type EncryptionRotationReport struct {
	Model          string
	OldKeyID       string
	NewKeyID       string
	StartedAfterID string
	LastID         string
	Scanned        int
	Rotated        int
	Failures       []EncryptionRotationFailure
	Complete       bool
}

// EncryptionRotationError reports every corrupt or inconsistent row found by
// a completed scan. Use errors.As to inspect Failures programmatically.
type EncryptionRotationError struct {
	Failures []EncryptionRotationFailure
}

func (e *EncryptionRotationError) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "maniflex.RotateEncryptionKey: rotation failed"
	}
	return fmt.Sprintf("maniflex.RotateEncryptionKey: %d row failure(s); first: %s",
		len(e.Failures), e.Failures[0].Error())
}

func (e *EncryptionRotationError) Unwrap() error {
	if e == nil || len(e.Failures) == 0 {
		return nil
	}
	errs := make([]error, 0, len(e.Failures))
	for _, failure := range e.Failures {
		errs = append(errs, failure.Err)
	}
	return errors.Join(errs...)
}

// RotateEncryptionKey is the compatibility wrapper around
// RotateEncryptionKeyWithOptions. Use the detailed function when a resumable
// cursor or row-addressable audit report is required.
func RotateEncryptionKey(ctx context.Context, s *Server, modelName, oldKeyID, newKeyID string) (int, error) {
	report, err := RotateEncryptionKeyWithOptions(
		ctx, s, modelName, oldKeyID, newKeyID, EncryptionRotationOptions{},
	)
	return report.Rotated, err
}

// RotateEncryptionKeyWithOptions re-encrypts a model's old-key envelopes using
// keyset pagination. Both encryption keys must remain available until Complete
// is true.
//
// Before writing anything, the function preflights the full requested range.
// Encrypted+unique fields require a dedicated blind-index key and their stored
// digest must already match it. The digest is not changed during rotation:
// equal plaintext therefore keeps the same UNIQUE value across old-key rows,
// new-key rows, and concurrent writes.
//
// If interrupted, old-key and new-key rows can coexist safely. Resume with
// AfterID set to the returned LastID. Rows already using newKeyID are
// idempotently ignored.
func RotateEncryptionKeyWithOptions(
	ctx context.Context,
	s *Server,
	modelName, oldKeyID, newKeyID string,
	opts EncryptionRotationOptions,
) (report EncryptionRotationReport, err error) {
	report = EncryptionRotationReport{
		Model:          modelName,
		OldKeyID:       oldKeyID,
		NewKeyID:       newKeyID,
		StartedAfterID: opts.AfterID,
		LastID:         opts.AfterID,
	}
	if s == nil {
		return report, fmt.Errorf("maniflex.RotateEncryptionKey: nil server")
	}
	meta, ok := s.registry.Get(modelName)
	if !ok {
		return report, fmt.Errorf("maniflex.RotateEncryptionKey: model %q not registered", modelName)
	}
	adapter := meta.ResolveAdapter(s.cfg.DB)
	if adapter == nil {
		return report, fmt.Errorf("maniflex.RotateEncryptionKey: no DB adapter configured for model %q", modelName)
	}
	kp := s.cfg.KeyProvider
	if kp == nil {
		return report, fmt.Errorf("maniflex.RotateEncryptionKey: no KeyProvider configured")
	}

	encFields := meta.EncryptedFields()
	if len(encFields) == 0 {
		report.Complete = true
		return report, nil
	}

	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = 100
	}
	if pageSize < 1 || pageSize > maxLimit {
		return report, fmt.Errorf("maniflex.RotateEncryptionKey: PageSize must be between 1 and %d", maxLimit)
	}

	hasUnique := false
	for _, f := range encFields {
		if f.Tags.Unique {
			hasUnique = true
		}
		configuredKeyID := f.Tags.EncryptionKey
		if configuredKeyID == "" {
			configuredKeyID = defaultEncryptionKey
		}
		if oldKeyID != newKeyID && configuredKeyID == oldKeyID {
			return report, fmt.Errorf(
				"maniflex.RotateEncryptionKey: field %q still writes with old key %q; configure it to use %q before online rotation",
				f.Tags.DBName, oldKeyID, newKeyID,
			)
		}
	}

	indexKeyID := ""
	if hasUnique {
		var stable bool
		indexKeyID, stable = blindIndexKeyID(kp, oldKeyID)
		if !stable {
			return report, fmt.Errorf(
				"maniflex.RotateEncryptionKey: model %q has encrypted unique fields but KeyProvider does not configure a stable blind-index key",
				modelName,
			)
		}
	}

	// Preflight the entire range before the first write. This makes malformed
	// envelopes and legacy rotating-key indexes visible without leaving a
	// partially changed database.
	preflight := report
	if err := scanEncryptionRotation(
		ctx, adapter, kp, meta, oldKeyID, indexKeyID, pageSize, false, &preflight,
	); err != nil {
		return preflight, err
	}
	if len(preflight.Failures) > 0 {
		preflight.LastID = opts.AfterID
		preflight.Rotated = 0
		return preflight, &EncryptionRotationError{Failures: preflight.Failures}
	}

	// Start the mutation scan from the caller's cursor again.
	report.Scanned = 0
	report.LastID = opts.AfterID
	if err := scanEncryptionRotation(
		ctx, adapter, kp, meta, oldKeyID, indexKeyID, pageSize, true, &report,
	); err != nil {
		return report, err
	}
	if len(report.Failures) > 0 {
		return report, &EncryptionRotationError{Failures: report.Failures}
	}
	report.Complete = true
	return report, nil
}

func scanEncryptionRotation(
	ctx context.Context,
	adapter DBAdapter,
	kp KeyProvider,
	meta *ModelMeta,
	oldKeyID, indexKeyID string,
	pageSize int,
	mutate bool,
	report *EncryptionRotationReport,
) error {
	lastID := report.LastID
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("maniflex.RotateEncryptionKey: interrupted after %q: %w", lastID, err)
		}
		qp := &QueryParams{
			Page:  1,
			Limit: pageSize,
			Sorts: []SortExpr{{DBName: "id", Direction: SortAsc}},
		}
		if lastID != "" {
			qp.Filters = []*FilterExpr{
				{Field: "id", Operator: OpGt, Value: lastID},
			}
		}
		recs, _, dbErr := adapter.FindMany(ctx, meta, qp)
		if dbErr != nil {
			return fmt.Errorf("maniflex.RotateEncryptionKey: FindMany after %q: %w", lastID, dbErr)
		}
		if len(recs) == 0 {
			break
		}

		for _, rec := range recs {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("maniflex.RotateEncryptionKey: interrupted after %q: %w", lastID, err)
			}
			row := recordToMap(meta, rec)
			id := fmt.Sprint(row["id"])
			update := map[string]any{}
			rowFailures := make([]EncryptionRotationFailure, 0)

			for _, f := range meta.EncryptedFields() {
				stored, ok := row[f.Tags.DBName].(string)
				if !ok || !strings.HasPrefix(stored, encStoragePrefix) {
					continue
				}
				envelope, decErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encStoragePrefix))
				if decErr != nil {
					rowFailures = append(rowFailures, EncryptionRotationFailure{
						RowID: id, Field: f.Tags.DBName, Stage: "decode-envelope", Err: decErr,
					})
					continue
				}
				keyID, kidErr := kp.KeyIDOf(envelope)
				if kidErr != nil {
					rowFailures = append(rowFailures, EncryptionRotationFailure{
						RowID: id, Field: f.Tags.DBName, Stage: "read-key-id", Err: kidErr,
					})
					continue
				}
				targetOldKey := keyID == oldKeyID
				// Unique fields are decrypted regardless of their envelope key
				// so preflight also catches an unsafe legacy digest on a row
				// already rotated by an earlier attempt.
				if !targetOldKey && !f.Tags.Unique {
					continue
				}

				plaintext, decErr := kp.Decrypt(ctx, envelope)
				if decErr != nil {
					rowFailures = append(rowFailures, EncryptionRotationFailure{
						RowID: id, Field: f.Tags.DBName, Stage: "decrypt", Err: decErr,
					})
					continue
				}

				if f.Tags.Unique {
					mac, macErr := kp.HMAC(ctx, indexKeyID, plaintext)
					if macErr != nil {
						rowFailures = append(rowFailures, EncryptionRotationFailure{
							RowID: id, Field: f.Tags.DBName, Stage: "blind-index", Err: macErr,
						})
						continue
					}
					want := base64.StdEncoding.EncodeToString(mac)
					var got string
					switch storedMAC := row[f.Tags.DBName+"_hmac"].(type) {
					case string:
						got = storedMAC
					case []byte:
						got = string(storedMAC)
					}
					if got != want {
						rowFailures = append(rowFailures, EncryptionRotationFailure{
							RowID: id,
							Field: f.Tags.DBName,
							Stage: "blind-index",
							Err:   fmt.Errorf("stored digest does not match stable blind-index key"),
						})
						continue
					}
				}

				if !targetOldKey || !mutate {
					continue
				}
				newEnv, encErr := kp.Encrypt(ctx, report.NewKeyID, plaintext)
				if encErr != nil {
					rowFailures = append(rowFailures, EncryptionRotationFailure{
						RowID: id, Field: f.Tags.DBName, Stage: "encrypt", Err: encErr,
					})
					continue
				}
				update[f.Tags.DBName] = encStoragePrefix + base64.StdEncoding.EncodeToString(newEnv)
			}

			report.Scanned++
			if len(rowFailures) > 0 {
				report.Failures = append(report.Failures, rowFailures...)
			} else if mutate && len(update) > 0 {
				upRec, mapErr := mapToRecord(meta, update)
				if mapErr != nil {
					return fmt.Errorf("maniflex.RotateEncryptionKey: map update row %v: %w", id, mapErr)
				}
				if _, upErr := adapter.Update(ctx, meta, id, upRec, presentDBKeys(update)); upErr != nil {
					return fmt.Errorf("maniflex.RotateEncryptionKey: update row %v: %w", id, upErr)
				}
				report.Rotated++
			}
			lastID = id
			report.LastID = id
		}

		if len(recs) < pageSize {
			break
		}
	}
	return nil
}

// abortEncryptionNotConfigured aborts the request with a clear error when a
// model has encrypted fields but no KeyProvider was configured.
func abortEncryptionNotConfigured(ctx *ServerContext, fieldName string) {
	ctx.Abort(http.StatusInternalServerError, "ENCRYPTION_NOT_CONFIGURED",
		fmt.Sprintf("field %q requires encryption but no KeyProvider is configured on the server", fieldName))
}
