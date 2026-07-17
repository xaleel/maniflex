package maniflex

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// DefaultMaxFileCount is the ceiling on how many keys a FileKeys field accepts
// when it carries no mfx:"max_count:N".
//
// Every key in the array is Stat'd against storage to enforce the field's rules,
// so an uncapped array is one client request costing N storage round-trips. That
// is the same shape as an unbounded fan-out, and the framework's other
// unbounded-input surfaces are capped by default rather than trusted
// (FilesConfig.MaxUploadBytes, Config.MaxConcurrentExports). Raise it per field
// with mfx:"max_count:N".
const DefaultMaxFileCount = 100

// FileKeys is a list of storage keys, usable as an mfx:"file" field so a model
// can hold many files in one column — a gallery, an attachment set.
//
//	type Post struct {
//	    maniflex.BaseModel
//	    Images maniflex.FileKeys `json:"images" mfx:"file,accept:image/*,max_size:5MB,auto_delete"`
//	}
//
// It stores as a JSON array (JSONB on Postgres, TEXT on SQLite), so ordering is
// preserved exactly as written — a gallery keeps its sequence. Every rule a
// single-key file field enforces applies per key: existence, max_size, accept,
// file_acl signing on read, auto_delete of superseded objects, and cleanup on
// hard delete.
//
// Writes are by key reference: upload via POST /files or a presigned upload
// (mfx:"upload:presigned"), then send the keys. Multipart carries one file per
// field, so it cannot populate an array — and routing many large files through
// the app process is what presigned uploads exist to avoid.
//
// A PATCH replaces the whole array, as it does any other column. With
// auto_delete, keys present before the write and absent after it are deleted
// from storage once the write commits.
type FileKeys []string

// SQLType implements SQLTyper: JSONB on Postgres (indexable), TEXT elsewhere.
// Mirrors LocaleString, the existing JSON-backed column type.
func (FileKeys) SQLType(driver DriverType) string {
	if driver == Postgres {
		return "JSONB"
	}
	return "TEXT"
}

// Schema implements ObjectWithSchema so the generated OpenAPI spec describes the
// column as an array of storage keys.
//
// Without it the field would be absent from the spec entirely: goTypeToSchema
// maps no slice kind and returns nil, which buildModelSchemas treats as "skip
// this field" — so a client generated from the spec would not know the column
// exists. ObjectWithSchema is the framework's own answer for exactly this.
func (FileKeys) Schema() *OASSchema {
	return &OASSchema{
		Type:  "array",
		Items: &OASSchema{Type: "string"},
	}
}

// Value implements driver.Valuer, storing the list as a JSON array.
//
// Implementing Valuer/Scanner rather than adding a case to the adapter's write
// normaliser keeps this type out of db/sqlcore: database/sql honours the
// interfaces natively, so the column works on any driver without the adapter
// knowing the type exists.
//
// A nil or empty list stores as "[]" rather than NULL, so a scanned column is
// always a valid JSON array and reads need no NULL branch.
func (fk FileKeys) Value() (driver.Value, error) {
	if len(fk) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal([]string(fk))
	if err != nil {
		return nil, fmt.Errorf("maniflex: marshal FileKeys: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (fk *FileKeys) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*fk = nil
		return nil
	case []byte:
		return fk.unmarshal(v)
	case string:
		return fk.unmarshal([]byte(v))
	default:
		return fmt.Errorf("maniflex: cannot scan %T into FileKeys", src)
	}
}

// fileKeysOfColumn returns every storage key a file column holds, whatever its
// shape: a single-key string field yields one, a FileKeys field yields its list,
// and a column read through a map path yields the list its stored JSON text
// encodes. An empty or unrecognised value yields none.
//
// It exists because a file column's key is read at four separate points — the
// existence/rule check, file_acl signing, auto_delete GC and hard-delete cleanup
// — each of which asserted .(string) and fell silently through on anything else.
// Normalising here is what stops the list shape being skipped by three of them.
func fileKeysOfColumn(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		// Could be a single key, or a FileKeys column carried as its stored JSON.
		if looksLikeJSONArray(t) {
			var out FileKeys
			if err := out.unmarshal([]byte(t)); err == nil {
				return out
			}
		}
		return []string{t}
	}
	if keys, ok := toFileKeys(v); ok {
		return keys
	}
	return nil
}

// looksLikeJSONArray reports whether s is plausibly a JSON array, so a stored
// FileKeys column is not mistaken for a single storage key. Storage keys are
// paths ("uploads/<uuid>/name.jpg"), never bracketed.
func looksLikeJSONArray(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}

func (fk *FileKeys) unmarshal(b []byte) error {
	if len(b) == 0 {
		*fk = nil
		return nil
	}
	var out []string
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("maniflex: scan FileKeys: %w", err)
	}
	*fk = out
	return nil
}
