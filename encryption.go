package maniflex

import (
	"context"
	"encoding/base64"
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

const (
	encStoragePrefix     = "enc:"
	defaultEncryptionKey = "default"
)

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
			mac, err := kp.HMAC(ctx, keyID, []byte(plaintext))
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

// RotateEncryptionKey re-encrypts every value of a model's encrypted fields
// from oldKeyID to newKeyID, one page of records at a time. Both keys must be
// active in the KeyProvider until rotation is confirmed complete.
//
// The operation is not atomic across all rows: if interrupted, rows encrypted
// with oldKeyID and rows encrypted with newKeyID will coexist. Both keys must
// remain available until the function returns without error.
//
// For large tables (millions of rows), run this as a background job (3C.3)
// rather than inline at startup.
//
// Pagination uses keyset (cursor) traversal — `WHERE id > $last ORDER BY id` —
// so each page covers fresh rows even as Update mutates the table and the
// underlying adapter's natural order is unstable (e.g. Postgres without an
// explicit ORDER BY). Offset pagination over a mutating set silently skipped
// or revisited rows.
func RotateEncryptionKey(ctx context.Context, s *Server, modelName, oldKeyID, newKeyID string) (rotated int, err error) {
	meta, ok := s.registry.Get(modelName)
	if !ok {
		return 0, fmt.Errorf("maniflex.RotateEncryptionKey: model %q not registered", modelName)
	}
	if s.cfg.DB == nil {
		return 0, fmt.Errorf("maniflex.RotateEncryptionKey: no DB adapter configured")
	}
	kp := s.cfg.KeyProvider
	if kp == nil {
		return 0, fmt.Errorf("maniflex.RotateEncryptionKey: no KeyProvider configured")
	}

	encFields := meta.EncryptedFields()
	if len(encFields) == 0 {
		return 0, nil
	}

	const pageSize = 100
	var lastID string
	for {
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
		recs, _, dbErr := s.cfg.DB.FindMany(ctx, meta, qp)
		if dbErr != nil {
			return rotated, fmt.Errorf("maniflex.RotateEncryptionKey: FindMany after %q: %w", lastID, dbErr)
		}
		if len(recs) == 0 {
			break
		}

		for _, rec := range recs {
			row := recordToMap(meta, rec)
			update := map[string]any{}

			for _, f := range encFields {
				stored, ok := row[f.Tags.DBName].(string)
				if !ok || !strings.HasPrefix(stored, encStoragePrefix) {
					continue
				}
				envelope, decErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encStoragePrefix))
				if decErr != nil {
					continue
				}
				keyID, kidErr := kp.KeyIDOf(envelope)
				if kidErr != nil || keyID != oldKeyID {
					continue
				}

				plaintext, decErr := kp.Decrypt(ctx, envelope)
				if decErr != nil {
					return rotated, fmt.Errorf("maniflex.RotateEncryptionKey: decrypt row %v field %q: %w",
						row["id"], f.Tags.DBName, decErr)
				}
				newEnv, encErr := kp.Encrypt(ctx, newKeyID, plaintext)
				if encErr != nil {
					return rotated, fmt.Errorf("maniflex.RotateEncryptionKey: encrypt row %v field %q: %w",
						row["id"], f.Tags.DBName, encErr)
				}
				update[f.Tags.DBName] = encStoragePrefix + base64.StdEncoding.EncodeToString(newEnv)

				if f.Tags.Unique {
					mac, macErr := kp.HMAC(ctx, newKeyID, plaintext)
					if macErr != nil {
						return rotated, fmt.Errorf("maniflex.RotateEncryptionKey: hmac row %v field %q: %w",
							row["id"], f.Tags.DBName, macErr)
					}
					update[f.Tags.DBName+"_hmac"] = base64.StdEncoding.EncodeToString(mac)
				}
			}

			id := fmt.Sprint(row["id"])
			if len(update) > 0 {
				upRec, _ := mapToRecord(meta, update)
				if _, upErr := s.cfg.DB.Update(ctx, meta, id, upRec, presentDBKeys(update)); upErr != nil {
					return rotated, fmt.Errorf("maniflex.RotateEncryptionKey: update row %v: %w", id, upErr)
				}
				rotated++
			}
			lastID = id
		}

		if len(recs) < pageSize {
			break
		}
	}
	return rotated, nil
}

// abortEncryptionNotConfigured aborts the request with a clear error when a
// model has encrypted fields but no KeyProvider was configured.
func abortEncryptionNotConfigured(ctx *ServerContext, fieldName string) {
	ctx.Abort(http.StatusInternalServerError, "ENCRYPTION_NOT_CONFIGURED",
		fmt.Sprintf("field %q requires encryption but no KeyProvider is configured on the server", fieldName))
}
