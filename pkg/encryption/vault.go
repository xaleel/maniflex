package encryption

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// VaultKeyProvider encrypts and decrypts via HashiCorp Vault's Transit secrets
// engine. The keyID maps to a Transit key name (e.g. "patient-pii").
//
// Vault manages key material, versioning, and rotation internally. This provider
// stores a thin envelope in the DB:
//
//	[ version:1 ][ keyIDLen:2 BE ][ keyID:N ][ vaultCiphertext:M ]
//
// The vaultCiphertext is the Vault-returned string (e.g. "vault:v1:..."), which
// embeds Vault's own key version, so decryption after a Vault key rotation is
// transparent — Vault handles both old and new key versions automatically.
//
// Authentication uses a static token. For production, wrap this provider and
// refresh the token from AppRole/Kubernetes auth before each operation.
//
// Usage:
//
//	server := maniflex.New(maniflex.Config{
//	    KeyProvider: &encryption.VaultKeyProvider{
//	        Address: "http://vault:8200",
//	        Token:   os.Getenv("VAULT_TOKEN"),
//	    },
//	})
type VaultKeyProvider struct {
	// Address is the Vault server URL. Required.
	Address string
	// Token is the Vault authentication token. Required.
	Token string
	// Mount is the Transit secrets engine path. Default: "transit".
	Mount string
	// Client is the HTTP client used for Vault calls. Defaults to http.DefaultClient.
	Client *http.Client
}

func (v *VaultKeyProvider) mount() string {
	if v.Mount != "" {
		return v.Mount
	}
	return "transit"
}

func (v *VaultKeyProvider) httpClient() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return http.DefaultClient
}

// Encrypt encrypts plaintext via Vault Transit and returns a binary envelope.
func (v *VaultKeyProvider) Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	body, _ := json.Marshal(map[string]string{"plaintext": encoded})

	url := fmt.Sprintf("%s/v1/%s/encrypt/%s", v.Address, v.mount(), keyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vault encrypt: build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", v.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault encrypt: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data   struct{ Ciphertext string `json:"ciphertext"` } `json:"data"`
		Errors []string                                        `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vault encrypt: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vault encrypt key %q: status %d: %s",
			keyID, resp.StatusCode, strings.Join(result.Errors, "; "))
	}

	return buildVaultEnvelope(keyID, []byte(result.Data.Ciphertext)), nil
}

// Decrypt decrypts a Vault envelope. It reads the keyID and Vault ciphertext
// from the envelope and sends them to the Vault Transit decrypt endpoint.
func (v *VaultKeyProvider) Decrypt(ctx context.Context, envelope []byte) ([]byte, error) {
	keyID, vaultCT, err := parseVaultEnvelope(envelope)
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]string{"ciphertext": string(vaultCT)})
	url := fmt.Sprintf("%s/v1/%s/decrypt/%s", v.Address, v.mount(), keyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", v.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data   struct{ Plaintext string `json:"plaintext"` } `json:"data"`
		Errors []string                                      `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vault decrypt: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vault decrypt key %q: status %d: %s",
			keyID, resp.StatusCode, strings.Join(result.Errors, "; "))
	}

	plaintext, err := base64.StdEncoding.DecodeString(result.Data.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: decode plaintext base64: %w", err)
	}
	return plaintext, nil
}

// KeyIDOf extracts the Vault Transit key name from an envelope without decrypting.
func (v *VaultKeyProvider) KeyIDOf(envelope []byte) (string, error) {
	keyID, _, err := parseVaultEnvelope(envelope)
	return keyID, err
}

// HMAC returns a Vault-keyed HMAC of data using the Transit HMAC endpoint.
// Vault's HMAC uses the named key so the digest is tied to key ownership.
func (v *VaultKeyProvider) HMAC(ctx context.Context, keyID string, data []byte) ([]byte, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	body, _ := json.Marshal(map[string]string{"input": encoded})

	url := fmt.Sprintf("%s/v1/%s/hmac/%s", v.Address, v.mount(), keyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vault hmac: build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", v.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault hmac: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data   struct{ Hmac string `json:"hmac"` } `json:"data"`
		Errors []string                            `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("vault hmac: decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vault hmac key %q: status %d: %s",
			keyID, resp.StatusCode, strings.Join(result.Errors, "; "))
	}
	// Vault returns "vault:v1:<base64>" — store the full opaque string as bytes
	return []byte(result.Data.Hmac), nil
}

// ── Vault envelope helpers ────────────────────────────────────────────────────

// buildVaultEnvelope packs keyID + Vault ciphertext into the shared envelope layout.
// Layout: [version:1][keyIDLen:2 BE][keyID:N][vaultCiphertext:M]
// No nonce field — Vault manages nonces internally.
func buildVaultEnvelope(keyID string, vaultCT []byte) []byte {
	keyIDBytes := []byte(keyID)
	env := make([]byte, 1+2+len(keyIDBytes)+len(vaultCT))
	off := 0
	env[off] = envelopeVersion
	off++
	binary.BigEndian.PutUint16(env[off:], uint16(len(keyIDBytes)))
	off += 2
	copy(env[off:], keyIDBytes)
	off += len(keyIDBytes)
	copy(env[off:], vaultCT)
	return env
}

// parseVaultEnvelope extracts the keyID and Vault ciphertext from an envelope.
func parseVaultEnvelope(env []byte) (keyID string, vaultCT []byte, err error) {
	if len(env) < 4 {
		return "", nil, fmt.Errorf("vault: envelope too short (%d bytes)", len(env))
	}
	if env[0] != envelopeVersion {
		return "", nil, fmt.Errorf("vault: unknown envelope version %d", env[0])
	}
	keyIDLen := int(binary.BigEndian.Uint16(env[1:3]))
	off := 3
	if len(env) < off+keyIDLen+1 {
		return "", nil, fmt.Errorf("vault: envelope too short for keyID (%d) + ciphertext", keyIDLen)
	}
	keyID = string(env[off : off+keyIDLen])
	off += keyIDLen
	vaultCT = env[off:]
	return keyID, vaultCT, nil
}
