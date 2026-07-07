package encryption

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"testing"
)

// SEC-10: EnvKeyProvider now binds the version byte and keyID into the AES-GCM
// tag as additional authenticated data. Verify the round-trip, that tampering
// with the bound metadata fails decryption, and that legacy v1 envelopes
// (sealed with nil AAD) still decrypt.
func TestEnvKeyProvider_AADBinding(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	t.Setenv("MFX_KEY_DEFAULT", base64.StdEncoding.EncodeToString(key))
	p := &EnvKeyProvider{}
	ctx := context.Background()

	// New envelopes are v2 and round-trip cleanly.
	env, err := p.Encrypt(ctx, "default", []byte("secret-value"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if env[0] != envelopeVersionGCMAAD {
		t.Fatalf("new envelope version = %d, want %d (v2)", env[0], envelopeVersionGCMAAD)
	}
	if pt, err := p.Decrypt(ctx, env); err != nil || string(pt) != "secret-value" {
		t.Fatalf("round-trip: plaintext=%q err=%v", pt, err)
	}

	// Tampering with the AAD-bound keyID must fail decryption. A second env var
	// holds the *same* key bytes under a different id, so loadKey succeeds and
	// the failure is unambiguously the AAD, not a missing key.
	t.Setenv("MFX_KEY_DEFAULX", base64.StdEncoding.EncodeToString(key))
	tampered := bytes.Clone(env)
	tampered[3+6] = 'x' // keyID "default" (offset 3, len 7) -> "defaulx"
	if _, err := p.Decrypt(ctx, tampered); err == nil {
		t.Error("decryption succeeded after tampering with the AAD-bound keyID; AAD not enforced")
	}

	// Downgrading the version byte (v2->v1) makes Decrypt use nil AAD, which no
	// longer matches the tag — a rollback attempt must fail.
	verFlip := bytes.Clone(env)
	verFlip[0] = envelopeVersion
	if _, err := p.Decrypt(ctx, verFlip); err == nil {
		t.Error("decryption succeeded after downgrading the version byte; version not bound")
	}

	// Backward compatibility: a genuine legacy v1 envelope (sealed with nil AAD)
	// must still decrypt.
	v1 := legacyV1Envelope(t, key, "default", []byte("legacy-value"))
	if pt, err := p.Decrypt(ctx, v1); err != nil || string(pt) != "legacy-value" {
		t.Fatalf("legacy v1 decrypt: plaintext=%q err=%v", pt, err)
	}
}

// legacyV1Envelope builds a version-1 envelope sealed with nil AAD, exactly as
// the pre-SEC-10 Encrypt did.
func legacyV1Envelope(t *testing.T, key []byte, keyID string, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil) // nil AAD == legacy v1

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
	return env
}
