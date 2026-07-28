package encryption

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultVaultTimeout bounds a complete Vault HTTP operation when neither the
// provider nor an injected client supplies a timeout.
const DefaultVaultTimeout = 10 * time.Second

// VaultTokenSource obtains a Vault token before each request. Implementations
// can cache and renew AppRole, Kubernetes, or other short-lived credentials.
type VaultTokenSource interface {
	Token(context.Context) (string, error)
}

// VaultTokenSourceFunc adapts a function into a VaultTokenSource.
type VaultTokenSourceFunc func(context.Context) (string, error)

// Token obtains a Vault token.
func (f VaultTokenSourceFunc) Token(ctx context.Context) (string, error) {
	return f(ctx)
}

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
//	        Address: "https://vault.example.com",
//	        Token:   os.Getenv("VAULT_TOKEN"),
//	    },
//	})
type VaultKeyProvider struct {
	// Address is the HTTPS Vault server URL. Required.
	Address string
	// Token is the static Vault authentication token. Required unless
	// TokenSource is set.
	Token string
	// TokenSource obtains a token before each request and takes precedence over
	// Token. Use it to integrate renewable AppRole or Kubernetes credentials.
	TokenSource VaultTokenSource
	// Mount is the Transit secrets engine path. Default: "transit".
	Mount string
	// Client is the HTTP client used for Vault calls. It is cloned before use.
	// The default is a private client with a bounded timeout.
	Client *http.Client
	// Timeout bounds the complete HTTP operation. Zero defaults to 10 seconds
	// unless an injected Client already has a timeout. A negative value
	// explicitly disables the client timeout; request context deadlines still
	// apply.
	Timeout time.Duration
	// AllowInsecureHTTP permits an http:// Address for explicitly isolated
	// development and test environments. It must not be enabled in production.
	AllowInsecureHTTP bool

	// IndexKeyID names a dedicated Transit key used only for HMAC blind indexes
	// on encrypted+unique fields. It must remain stable as encryption keys rotate.
	IndexKeyID string
}

// BlindIndexKeyID advertises the rotation-independent HMAC key to maniflex.
func (v *VaultKeyProvider) BlindIndexKeyID() string { return v.IndexKeyID }

func (v *VaultKeyProvider) mount() string {
	if v.Mount != "" {
		return v.Mount
	}
	return "transit"
}

func (v *VaultKeyProvider) httpClient() *http.Client {
	client := &http.Client{}
	if v.Client != nil {
		*client = *v.Client
	}
	switch {
	case v.Timeout < 0:
		client.Timeout = 0
	case v.Timeout > 0:
		client.Timeout = v.Timeout
	case client.Timeout == 0:
		client.Timeout = DefaultVaultTimeout
	}
	return client
}

func (v *VaultKeyProvider) endpoint(action, keyID string) (string, error) {
	base, err := url.Parse(v.Address)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("vault: invalid Address %q", v.Address)
	}
	switch strings.ToLower(base.Scheme) {
	case "https":
	case "http":
		if !v.AllowInsecureHTTP {
			return "", fmt.Errorf("vault: insecure Address %q requires AllowInsecureHTTP", v.Address)
		}
	default:
		return "", fmt.Errorf("vault: Address must use https, got %q", base.Scheme)
	}
	return fmt.Sprintf("%s/v1/%s/%s/%s",
		strings.TrimRight(v.Address, "/"), v.mount(), action, keyID), nil
}

func (v *VaultKeyProvider) token(ctx context.Context) (string, error) {
	token := v.Token
	if v.TokenSource != nil {
		var err error
		token, err = v.TokenSource.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("vault: get token: %w", err)
		}
	}
	if token == "" {
		return "", fmt.Errorf("vault: Token or TokenSource is required")
	}
	return token, nil
}

func (v *VaultKeyProvider) request(
	ctx context.Context,
	operation, action, keyID string,
	body []byte,
) (*http.Request, error) {
	endpoint, err := v.endpoint(action, keyID)
	if err != nil {
		return nil, fmt.Errorf("vault %s: %w", operation, err)
	}
	token, err := v.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault %s: %w", operation, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vault %s: build request: %w", operation, err)
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// Encrypt encrypts plaintext via Vault Transit and returns a binary envelope.
func (v *VaultKeyProvider) Encrypt(ctx context.Context, keyID string, plaintext []byte) ([]byte, error) {
	encoded := base64.StdEncoding.EncodeToString(plaintext)
	body, _ := json.Marshal(map[string]string{"plaintext": encoded})

	req, err := v.request(ctx, "encrypt", "encrypt", keyID, body)
	if err != nil {
		return nil, err
	}

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault encrypt: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
		Errors []string `json:"errors"`
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
	req, err := v.request(ctx, "decrypt", "decrypt", keyID, body)
	if err != nil {
		return nil, err
	}

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault decrypt: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
		Errors []string `json:"errors"`
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

	req, err := v.request(ctx, "hmac", "hmac", keyID, body)
	if err != nil {
		return nil, err
	}

	resp, err := v.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault hmac: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Hmac string `json:"hmac"`
		} `json:"data"`
		Errors []string `json:"errors"`
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
