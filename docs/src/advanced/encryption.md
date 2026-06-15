# Encryption at Rest

A field tagged `mfx:"encrypted"` is automatically encrypted before it
reaches the database and decrypted on read. The plaintext never appears in
the table; the column stores a self-describing envelope. This page covers
the full subsystem: the tag, the key provider interface, the storage
format, unique-constraint handling, and key rotation.

## Declaring an encrypted field

```go
type Patient struct {
    maniflex.BaseModel
    Name string `json:"name" mfx:"required,filterable,sortable"`
    SSN  string `json:"ssn"  mfx:"encrypted,key:patient-pii"`
}
```

| Sub-option | Effect |
|---|---|
| `encrypted` | mark the field for envelope encryption |
| `key:NAME` | the key identifier passed to the `KeyProvider`; defaults to `"default"` |

The column's Go and DB types remain `string`. Storage is the prefix
`enc:` followed by a base64-encoded binary envelope:

```
enc:Aa1z...   (envelope bytes embed the keyID)
```

The `enc:` prefix lets the framework distinguish ciphertext from any
legacy plaintext that may exist in the column — useful for incremental
migration of an existing table.

## What encryption costs you

The trade-offs are deliberate and worth being explicit about:

- **No filtering.** A `WHERE ssn = ?` would have to match an envelope
  that includes a random nonce. Encrypted fields cannot be `filterable`.
- **No sorting.** Same reason. Encrypted fields cannot be `sortable`.
- **Uniqueness via HMAC.** A `mfx:"encrypted,unique"` field gets a
  companion `{field}_hmac` `TEXT UNIQUE` column. See the next section.
- **The KeyProvider is required.** Reads degrade to returning the raw
  stored ciphertext; writes are rejected with
  `500 ENCRYPTION_NOT_CONFIGURED` until a provider is configured.

For columns that need to be queryable but contain sensitive data, store a
non-sensitive lookup key (a hashed identifier) in a separate field and
encrypt only the payload.

## Configuring a KeyProvider

`maniflex.Config.KeyProvider` must be set before any model with encrypted
fields is exercised. Two implementations ship in `pkg/encryption`, both
constructed as plain struct literals.

### `EnvKeyProvider` — keys from environment variables

```go
import "maniflex/pkg/encryption"

server := maniflex.New(maniflex.Config{
    KeyProvider: &encryption.EnvKeyProvider{Prefix: "MYAPP_KEY"},
    // ...
})
```

The env var name for a given `keyID` is derived as
`{Prefix}_{KEYID_UPPER}`, with hyphens replaced by underscores and the
result uppercased:

| `Prefix` | `keyID` | Env var read |
|---|---|---|
| `MYAPP_KEY` | `default` | `MYAPP_KEY_DEFAULT` |
| `MYAPP_KEY` | `patient-pii` | `MYAPP_KEY_PATIENT_PII` |
| `MFX_KEY` (default) | `billing` | `MFX_KEY_BILLING` |

Each variable holds a base64-encoded 32-byte (256-bit) AES key. Generate
one with:

```bash
openssl rand -base64 32
```

The provider accepts either standard or URL-safe base64.

### `VaultKeyProvider` — HashiCorp Vault Transit

```go
server := maniflex.New(maniflex.Config{
    KeyProvider: &encryption.VaultKeyProvider{
        Address: "https://vault.example.com",
        Token:   os.Getenv("VAULT_TOKEN"),
        Mount:   "transit",            // optional, default "transit"
        // Client: customHTTPClient,    // optional, defaults to http.DefaultClient
    },
})
```

`keyID` maps to a Vault Transit key name; the plaintext is sent to
`/v1/{mount}/encrypt/{keyID}` and Vault returns its own
`vault:v1:...` ciphertext, which the provider embeds in the envelope.
A Vault key rotation is transparent — Vault decrypts ciphertexts
encrypted with any prior version of the key automatically.

The shipped provider uses a static token. For production, wrap it with
a refresher that obtains a fresh token from AppRole, Kubernetes auth,
or JWT auth before each operation.

A custom backend implements the `maniflex.KeyProvider` interface:

```go
type KeyProvider interface {
    Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error)
    Decrypt(ctx context.Context, envelope []byte) ([]byte, error)
    KeyIDOf(envelope []byte) (string, error)
    HMAC(ctx context.Context, keyID string, data []byte) ([]byte, error)
}
```

`Encrypt` returns a self-describing binary envelope that embeds the
`keyID`. `Decrypt` reads the keyID from the envelope, so callers don't
supply it. `HMAC` produces a deterministic keyed digest used for unique
indexes.

## Unique encrypted fields

A normal `UNIQUE` constraint on an envelope is useless — each envelope
contains a random nonce, so two encryptions of the same plaintext are
different ciphertexts. The framework solves this with an HMAC companion
column.

```go
Email string `json:"email" mfx:"encrypted,unique"`
```

`AutoMigrate` emits two columns:

| Column | Type | Purpose |
|---|---|---|
| `email` | TEXT | the `enc:<base64>` envelope |
| `email_hmac` | TEXT UNIQUE | a keyed HMAC of the plaintext |

On every write, the DB step calls `KeyProvider.HMAC(ctx, keyID, plaintext)`
and stores the result in the companion. The HMAC is deterministic for a
given (key, plaintext) pair, so the database can enforce uniqueness
without ever seeing the plaintext.

Reads strip the HMAC column from responses automatically; clients see only
the decrypted plaintext on `email` and never the digest.

When the unique check fires, the adapter returns `*maniflex.ErrConstraint` and
the DB step converts it to `409 CONFLICT` — same path as any other unique
violation.

## Per-domain keys

The `key:NAME` sub-option routes a field to a specific key identifier:

```go
type Record struct {
    maniflex.BaseModel
    PaymentToken string `json:"payment_token" mfx:"encrypted,key:billing"`
    MedicalNote  string `json:"medical_note"  mfx:"encrypted,key:medical"`
}
```

A KeyProvider that backs different keys with different secrets (or a Vault
transit mount) lets you scope access by domain — the billing team holds
the billing key; medical staff hold the medical key; the application
process holds both. Rotating one does not affect the other.

When `key:` is omitted, the framework uses the keyID `"default"`. Either
configure a key under that name or always tag with an explicit key.

## Decryption on the read path

The DB step runs the decryption pass after every read:

- For list and read operations, `decryptFields` replaces every
  `enc:<base64>` value with the decrypted plaintext.
- HMAC companion columns are always stripped from the response.
- Values that do not have the `enc:` prefix are left as-is — important
  for *gradual* adoption: enable encryption on a column whose existing
  rows are plaintext, and only new writes get encrypted.

If `KeyProvider` is nil but a model has encrypted fields, reads return
the raw stored ciphertext (so the application still functions in some
read-only sense), but writes are refused. Configuring a provider is the
only way to write encrypted columns.

## Key rotation

`maniflex.RotateEncryptionKey(ctx, server, modelName, oldKeyID, newKeyID)`
re-encrypts every row of a model whose envelopes were encrypted with
`oldKeyID`:

```go
n, err := maniflex.RotateEncryptionKey(ctx, server, "Patient", "v1", "v2")
if err != nil {
    log.Fatal(err)
}
log.Printf("re-encrypted %d rows", n)
```

The function pages through the table 100 rows at a time, decrypts each
value with the old key, re-encrypts with the new key, and updates the
HMAC companion for any unique encrypted fields. Both keys must remain
available in the `KeyProvider` until the rotation completes — partial
rotations leave the table with a mix of old-key and new-key envelopes
until you finish.

The operation is not atomic across all rows. On failure, run it again —
it skips envelopes whose keyID already matches `newKeyID`, so a partial
rotation is safe to resume.

For large tables, run the rotation as a background job rather than at
startup. Each row is a separate `UPDATE`, so the operation is bound by
the database's write throughput.

## What an envelope looks like

The exact envelope format is the `KeyProvider`'s concern. The two
shipped providers use slightly different layouts, but both put a
self-describing header in front of the ciphertext so `KeyIDOf` can
extract the keyID without decrypting.

**`EnvKeyProvider`** — AES-256-GCM with an inline nonce:

```
[ version:1 ][ keyIDLen:2 (BE) ][ keyID:N ][ nonce:12 ][ gcmCiphertext+tag:M ]
```

**`VaultKeyProvider`** — Vault returns a `vault:v1:...` ciphertext that
embeds its own versioning, so the envelope carries no nonce:

```
[ version:1 ][ keyIDLen:2 (BE) ][ keyID:N ][ vaultCiphertext:M ]
```

In both cases `version` is `0x01` and the keyID length is a 16-bit
big-endian integer. The framework stores the binary envelope as the
string `enc:<base64>` in the column.

`Encrypt` produces the blob; `Decrypt` parses the header to recover the
keyID, then routes to the right key. `KeyIDOf` reads the keyID without
decrypting — useful for audit logging and for the rotation loop above.

A custom provider need not follow either format; the framework only
cares that `Encrypt` and `Decrypt` are inverses and that `KeyIDOf` works
on the output of `Encrypt`.

## Compatibility with other features

| Feature | Interaction |
|---|---|
| `mfx:"encrypted"` + `unique` | HMAC companion column; standard unique violation as `409` |
| `mfx:"encrypted"` + `filterable` / `sortable` | not allowed — filterable/sortable tags are silently dropped at scan time |
| `mfx:"encrypted"` + soft-delete | independent — soft-delete operates on a separate marker column |
| `mfx:"encrypted"` + versioning | encrypted fields are *excluded* from `diff` and `snapshot`. History rows record metadata only, not plaintexts |
| `mfx:"encrypted"` + audit log | the audit `Changes` diff excludes encrypted fields by default; use `WithExcludeFields` to add more |
| `mfx:"encrypted"` + relations | a relation FK is never encrypted; relation joins remain unaffected |

## Operational checklist

- Set `Config.KeyProvider` before any encrypted-field model is registered.
- Back keys with a secret store (env vars from a vault, HashiCorp Vault
  Transit, AWS KMS). Never commit a key to source control.
- For staged rollout, deploy the schema (`email_hmac` column) before the
  application change that starts encrypting — and the application change
  before the migration that backfills existing rows.
- Keep both keys active throughout a rotation; remove the old key only
  after `RotateEncryptionKey` has reported every row migrated.
- Treat `KeyIDOf(envelope)` as the source of truth for "which key
  encrypted this row" — useful for auditing the rotation.
