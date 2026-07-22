package sql

// Audit JB-14: the payload cipher carried no key id, so rotating the key made
// every job still holding a payload encrypted under the old one undecodable —
// they are quarantined as dead rather than run (JB-11). And unmarshalPayload
// returned the stored value verbatim whenever no cipher was configured, so
// dropping the option handed handlers the literal string "encq:<hex>" as their
// JSON payload: ciphertext in place of data, with no error to notice.
//
// WithKeyProvider now stores maniflex's self-describing envelope ("enc:" +
// base64), which embeds the key id, so the provider decrypts an old payload with
// the key it was written under. These tests drive the real EnvKeyProvider — the
// same one struct-field encryption uses — so the rotation is genuine, not a fake.
//
//	go test ./jobs/sql/ -run TestPayload

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xaleel/maniflex/pkg/encryption"
)

// envProvider installs a random AES-256 key for each keyID under prefix and
// returns a provider over them. EnvKeyProvider derives the env var name as
// {Prefix}_{KEYID_UPPER} with hyphens mapped to underscores.
func envProvider(t *testing.T, prefix string, keyIDs ...string) *encryption.EnvKeyProvider {
	t.Helper()
	for _, id := range keyIDs {
		var k [32]byte
		if _, err := rand.Read(k[:]); err != nil {
			t.Fatalf("key material: %v", err)
		}
		t.Setenv(prefix+"_"+strings.ToUpper(strings.ReplaceAll(id, "-", "_")),
			base64.StdEncoding.EncodeToString(k[:]))
	}
	return &encryption.EnvKeyProvider{Prefix: prefix}
}

// xorCipher is a stand-in for the legacy keyless PayloadCipher.
type xorCipher struct{ mask byte }

func (c xorCipher) apply(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ c.mask
	}
	return out
}
func (c xorCipher) Encrypt(p []byte) ([]byte, error) { return c.apply(p), nil }
func (c xorCipher) Decrypt(p []byte) ([]byte, error) { return c.apply(p), nil }

var samplePayload = json.RawMessage(`{"invoice":"INV-1","amount":420}`)

func TestPayload_KeyProviderRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := &Queue{kp: envProvider(t, "MFXJOBTEST", "k1"), keyID: "k1"}

	stored, err := q.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(stored, encEnvelopePrefix) {
		t.Errorf("stored value %q does not use the envelope prefix %q", stored, encEnvelopePrefix)
	}
	if strings.Contains(stored, "INV-1") {
		t.Error("payload is readable in the stored value")
	}

	got, err := q.unmarshalPayload(ctx, stored)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got) != string(samplePayload) {
		t.Errorf("round-trip = %s, want %s", got, samplePayload)
	}
}

// The headline: a job written under the previous key still decrypts after the
// queue has moved on to a new one, because the id travels in the envelope.
func TestPayload_RotatedKeyStillDecryptsOldPayload(t *testing.T) {
	ctx := context.Background()
	kp := envProvider(t, "MFXJOBTEST", "k1", "k2")

	before := &Queue{kp: kp, keyID: "k1"} // the queue as it was
	stored, err := before.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal under k1: %v", err)
	}

	after := &Queue{kp: kp, keyID: "k2"} // same provider, now writing under k2
	got, err := after.unmarshalPayload(ctx, stored)
	if err != nil {
		t.Fatalf("a job enqueued under the previous key no longer decrypts after "+
			"rotation — this is the JB-14 failure: %v", err)
	}
	if string(got) != string(samplePayload) {
		t.Errorf("decrypted %s, want %s", got, samplePayload)
	}

	// And new work is written under the new key, not the old one.
	fresh, err := after.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal under k2: %v", err)
	}
	if fresh == stored {
		t.Error("payload written under k2 is byte-identical to the k1 one")
	}
	if _, err := after.unmarshalPayload(ctx, fresh); err != nil {
		t.Errorf("payload written under the current key does not decrypt: %v", err)
	}
}

// A key the provider can no longer resolve is an error, not a payload.
func TestPayload_UnresolvableKeyIsAnError(t *testing.T) {
	ctx := context.Background()
	written := &Queue{kp: envProvider(t, "MFXJOBHAVE", "k1"), keyID: "k1"}
	stored, err := written.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// A provider with no key material at all: the id in the envelope resolves to
	// nothing, the way a key retired too early would.
	orphaned := &Queue{kp: &encryption.EnvKeyProvider{Prefix: "MFXJOBGONE"}, keyID: "k1"}
	if got, err := orphaned.unmarshalPayload(ctx, stored); err == nil {
		t.Errorf("a payload whose key is gone decoded to %s, want an error", got)
	}
}

// The garbage-passthrough bug, both formats: an encrypted value read with no way
// to decrypt it must be an error, never handed back as if it were the payload.
func TestPayload_EncryptedWithoutTheOptionIsAnError(t *testing.T) {
	ctx := context.Background()
	plain := &Queue{} // neither WithKeyProvider nor WithPayloadCipher

	for _, stored := range []string{
		encEnvelopePrefix + base64.StdEncoding.EncodeToString([]byte("envelope-bytes")),
		encPayloadPrefix + "deadbeef",
	} {
		got, err := plain.unmarshalPayload(ctx, stored)
		if err == nil {
			t.Errorf("stored %q decoded to %s with no error — that string would reach "+
				"the handler as its payload", stored, got)
		}
		if got != nil {
			t.Errorf("stored %q returned a payload (%s) alongside its error", stored, got)
		}
	}
}

// Anti-regression: the legacy keyless format still round-trips for anyone using it.
func TestPayload_LegacyCipherRoundTrip(t *testing.T) {
	ctx := context.Background()
	q := &Queue{cipher: xorCipher{mask: 0x5A}}

	stored, err := q.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(stored, encPayloadPrefix) {
		t.Errorf("stored %q lost the legacy prefix", stored)
	}
	got, err := q.unmarshalPayload(ctx, stored)
	if err != nil || string(got) != string(samplePayload) {
		t.Errorf("legacy round-trip = (%s, %v), want the payload back", got, err)
	}
}

// A queue migrating from the cipher to a provider: new rows go through the
// provider, existing "encq:" rows still decrypt.
func TestPayload_ProviderWritesWhileCipherStillReads(t *testing.T) {
	ctx := context.Background()
	cipher := xorCipher{mask: 0x5A}

	legacyStored, err := (&Queue{cipher: cipher}).marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("legacy marshal: %v", err)
	}

	migrating := &Queue{kp: envProvider(t, "MFXJOBTEST", "k1"), keyID: "k1", cipher: cipher}

	fresh, err := migrating.marshalPayload(ctx, samplePayload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.HasPrefix(fresh, encEnvelopePrefix) {
		t.Errorf("new write used %q, want the provider envelope", fresh)
	}
	if got, err := migrating.unmarshalPayload(ctx, legacyStored); err != nil ||
		string(got) != string(samplePayload) {
		t.Errorf("existing legacy row no longer readable during migration: (%s, %v)", got, err)
	}
}

// Unprefixed rows are cleartext (pre-encryption legacy) and pass through.
func TestPayload_CleartextPassthrough(t *testing.T) {
	ctx := context.Background()
	for _, q := range []*Queue{
		{},
		{cipher: xorCipher{mask: 0x5A}},
		{kp: envProvider(t, "MFXJOBTEST", "k1"), keyID: "k1"},
	} {
		got, err := q.unmarshalPayload(ctx, string(samplePayload))
		if err != nil || string(got) != string(samplePayload) {
			t.Errorf("cleartext passthrough = (%s, %v), want the payload back", got, err)
		}
	}
}
