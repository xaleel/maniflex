package encryption

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type vaultRoundTripFunc func(*http.Request) (*http.Response, error)

func (f vaultRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func vaultResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestVaultKeyProviderAppliesDefaultTimeoutWithoutMutatingClient(t *testing.T) {
	var deadline time.Time
	client := &http.Client{
		Transport: vaultRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			deadline, _ = req.Context().Deadline()
			if got := req.Header.Get("X-Vault-Token"); got != "static-token" {
				t.Errorf("X-Vault-Token = %q, want static token", got)
			}
			return vaultResponse(req, `{"data":{"ciphertext":"vault:v1:test"}}`), nil
		}),
	}
	provider := &VaultKeyProvider{
		Address: "https://vault.example.com",
		Token:   "static-token",
		Client:  client,
	}

	if _, err := provider.Encrypt(context.Background(), "patient-pii", []byte("secret")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	remaining := time.Until(deadline)
	if deadline.IsZero() || remaining < 9*time.Second || remaining > DefaultVaultTimeout {
		t.Errorf("default deadline remaining = %v, want approximately %v", remaining, DefaultVaultTimeout)
	}
	if client.Timeout != 0 {
		t.Errorf("injected client mutated: Timeout = %v", client.Timeout)
	}
}

func TestVaultKeyProviderRejectsHTTPUnlessExplicitlyAllowed(t *testing.T) {
	var calls int32
	client := &http.Client{
		Transport: vaultRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return vaultResponse(req, `{"data":{"ciphertext":"vault:v1:test"}}`), nil
		}),
	}
	provider := &VaultKeyProvider{
		Address: "http://vault:8200",
		Token:   "test-token",
		Client:  client,
	}

	if _, err := provider.Encrypt(context.Background(), "key", []byte("secret")); err == nil ||
		!strings.Contains(err.Error(), "AllowInsecureHTTP") {
		t.Fatalf("Encrypt error = %v, want insecure-address rejection", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("transport calls = %d before insecure address was allowed", got)
	}

	provider.AllowInsecureHTTP = true
	if _, err := provider.Encrypt(context.Background(), "key", []byte("secret")); err != nil {
		t.Fatalf("Encrypt with explicit development opt-out: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("transport calls = %d, want 1", got)
	}
}

func TestVaultKeyProviderTokenSourceRunsBeforeEveryRequest(t *testing.T) {
	var sourceCalls int32
	var gotTokens []string
	provider := &VaultKeyProvider{
		Address: "https://vault.example.com",
		Token:   "stale-static-token",
		TokenSource: VaultTokenSourceFunc(func(context.Context) (string, error) {
			n := atomic.AddInt32(&sourceCalls, 1)
			return "renewed-" + string(rune('0'+n)), nil
		}),
		Client: &http.Client{
			Transport: vaultRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				gotTokens = append(gotTokens, req.Header.Get("X-Vault-Token"))
				if strings.Contains(req.URL.Path, "/hmac/") {
					return vaultResponse(req, `{"data":{"hmac":"vault:v1:dGVzdA=="}}`), nil
				}
				return vaultResponse(req, `{"data":{"ciphertext":"vault:v1:test"}}`), nil
			}),
		},
	}

	if _, err := provider.Encrypt(context.Background(), "key", []byte("secret")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := provider.HMAC(context.Background(), "index", []byte("value")); err != nil {
		t.Fatalf("HMAC: %v", err)
	}
	if got := atomic.LoadInt32(&sourceCalls); got != 2 {
		t.Fatalf("token source calls = %d, want 2", got)
	}
	if len(gotTokens) != 2 || gotTokens[0] != "renewed-1" || gotTokens[1] != "renewed-2" {
		t.Errorf("request tokens = %#v, want renewed token per request", gotTokens)
	}
}

func TestVaultKeyProviderTokenSourceErrorStopsRequest(t *testing.T) {
	sourceErr := errors.New("renewal failed")
	var transportCalled bool
	provider := &VaultKeyProvider{
		Address: "https://vault.example.com",
		TokenSource: VaultTokenSourceFunc(func(context.Context) (string, error) {
			return "", sourceErr
		}),
		Client: &http.Client{
			Transport: vaultRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				transportCalled = true
				return vaultResponse(req, `{}`), nil
			}),
		},
	}

	_, err := provider.Encrypt(context.Background(), "key", []byte("secret"))
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Encrypt error = %v, want token source error", err)
	}
	if transportCalled {
		t.Fatal("transport called after token source failure")
	}
}

func TestVaultKeyProviderNegativeTimeoutLeavesDeadlineToContext(t *testing.T) {
	var hasDeadline bool
	provider := &VaultKeyProvider{
		Address: "https://vault.example.com",
		Token:   "test-token",
		Timeout: -1,
		Client: &http.Client{
			Transport: vaultRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				_, hasDeadline = req.Context().Deadline()
				return vaultResponse(req, `{"data":{"ciphertext":"vault:v1:test"}}`), nil
			}),
		},
	}

	if _, err := provider.Encrypt(context.Background(), "key", []byte("secret")); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if hasDeadline {
		t.Fatal("negative Timeout unexpectedly added a deadline")
	}
}
