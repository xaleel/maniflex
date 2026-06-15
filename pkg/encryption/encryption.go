// Package encryption provides KeyProvider implementations for mfx:"encrypted"
// struct fields. The two bundled implementations are EnvKeyProvider (keys from
// environment variables) and VaultKeyProvider (HashiCorp Vault Transit engine).
//
// Envelope format produced by EnvKeyProvider:
//
//	[ version:1 ][ keyIDLen:2 BE ][ keyID:N ][ nonce:12 ][ gcmCiphertext:M ]
//
// Stored in the DB as the string  "enc:<base64(envelope)>".
// The "enc:" prefix lets the framework distinguish already-encrypted values
// from legacy plaintext when a model is being migrated to encryption.
package encryption

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

const envelopeVersion byte = 1

// EnvKeyProvider loads AES-256-GCM keys from environment variables.
//
// The env var name is built as:  {Prefix}_{KEYID_UPPER}
// where hyphens in keyID are replaced with underscores and everything is
// uppercased. For example:
//
//	Prefix "MFX_KEY" + keyID "patient-pii"  →  MFX_KEY_PATIENT_PII
//	Prefix "MFX_KEY" + keyID "default"      →  MFX_KEY_DEFAULT
//
// Each env var must contain a base64-encoded 32-byte (256-bit) key.
// Generate one with:
//
//	openssl rand -base64 32
//
// Usage:
//
//	server := maniflex.New(maniflex.Config{
//	    KeyProvider: &encryption.EnvKeyProvider{Prefix: "MYAPP_KEY"},
//	})
type EnvKeyProvider struct {
	// Prefix is prepended to the keyID when building the env var name.
	// Default: "MFX_KEY".
	Prefix string
}

func (p *EnvKeyProvider) prefix() string {
	if p.Prefix != "" {
		return p.Prefix
	}
	return "MFX_KEY"
}

func (p *EnvKeyProvider) envVarName(keyID string) string {
	upper := strings.ToUpper(strings.ReplaceAll(keyID, "-", "_"))
	return p.prefix() + "_" + upper
}

func (p *EnvKeyProvider) loadKey(keyID string) ([]byte, error) {
	name := p.envVarName(keyID)
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("encryption: key %q not set (env var %s)", keyID, name)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		key, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("encryption: key %q in %s: invalid base64: %w", keyID, name, err)
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption: key %q in %s: must be 32 bytes (got %d)", keyID, name, len(key))
	}
	return key, nil
}

// Encrypt encrypts plaintext with AES-256-GCM and returns a binary envelope.
// The envelope is self-describing: it embeds the keyID so that Decrypt never
// needs the caller to supply a key name.
func (p *EnvKeyProvider) Encrypt(_ context.Context, keyID string, plaintext []byte) ([]byte, error) {
	key, err := p.loadKey(keyID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryption: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encryption: generate nonce: %w", err)
	}

	ct := gcm.Seal(nil, nonce, plaintext, nil)

	keyIDBytes := []byte(keyID)
	env := make([]byte, 1+2+len(keyIDBytes)+len(nonce)+len(ct))
	off := 0
	env[off] = envelopeVersion
	off++
	binary.BigEndian.PutUint16(env[off:], uint16(len(keyIDBytes)))
	off += 2
	copy(env[off:], keyIDBytes)
	off += len(keyIDBytes)
	copy(env[off:], nonce)
	off += len(nonce)
	copy(env[off:], ct)
	return env, nil
}

// Decrypt decrypts an envelope produced by Encrypt. It reads the keyID from
// the envelope, loads the corresponding env var key, and decrypts with AES-256-GCM.
func (p *EnvKeyProvider) Decrypt(_ context.Context, envelope []byte) ([]byte, error) {
	keyID, nonce, ct, err := parseEnvelope(envelope)
	if err != nil {
		return nil, err
	}

	key, err := p.loadKey(keyID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encryption: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encryption: create GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("encryption: decrypt with key %q: %w", keyID, err)
	}
	return plaintext, nil
}

// KeyIDOf extracts the keyID from an envelope without decrypting it.
func (p *EnvKeyProvider) KeyIDOf(envelope []byte) (string, error) {
	keyID, _, _, err := parseEnvelope(envelope)
	return keyID, err
}

// HMAC returns HMAC-SHA256 of data using the named key. Used to enforce UNIQUE
// constraints on encrypted fields without storing plaintext.
func (p *EnvKeyProvider) HMAC(_ context.Context, keyID string, data []byte) ([]byte, error) {
	key, err := p.loadKey(keyID)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil), nil
}

// ── Envelope helpers ──────────────────────────────────────────────────────────

// parseEnvelope splits a binary envelope into its components.
// Layout: [version:1][keyIDLen:2 BE][keyID:N][nonce:12][ciphertext:M]
func parseEnvelope(env []byte) (keyID string, nonce, ciphertext []byte, err error) {
	const minLen = 1 + 2 + 0 + 12 + 1 // version + keyIDLen + keyID(0) + nonce + 1 byte ct
	if len(env) < minLen {
		return "", nil, nil, fmt.Errorf("encryption: envelope too short (%d bytes)", len(env))
	}
	if env[0] != envelopeVersion {
		return "", nil, nil, fmt.Errorf("encryption: unknown envelope version %d", env[0])
	}
	keyIDLen := int(binary.BigEndian.Uint16(env[1:3]))
	off := 3
	if len(env) < off+keyIDLen+12+1 {
		return "", nil, nil, fmt.Errorf("encryption: envelope too short for keyID (%d) + nonce + ciphertext", keyIDLen)
	}
	keyID = string(env[off : off+keyIDLen])
	off += keyIDLen
	nonce = env[off : off+12]
	off += 12
	ciphertext = env[off:]
	return keyID, nonce, ciphertext, nil
}
