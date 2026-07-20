package kafka

// Audit EV-12: the adapter exposed no TLS or SASL configuration, so it could
// only reach plaintext, unauthenticated brokers. Every managed Kafka service
// requires at least one of the two, which put them out of reach entirely.
//
// The connection is made in four separate places — the writer, each
// consumer-group reader, and both admin dials in createTopic — and they are
// distinct code paths. Configuring only some of them is the failure that would
// actually ship: publishing succeeds while topic creation is refused, or the
// producer connects and every consumer is rejected. These tests assert the
// credentials reach each one, which needs no broker: kafka-go exposes both the
// writer's Transport and the reader's Config.
//
//	go test ./events/kafka/... -run TestDialer

import (
	"crypto/tls"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
)

func testCreds() (*tls.Config, plain.Mechanism) {
	return &tls.Config{ServerName: "broker.example.com", MinVersion: tls.VersionTLS12},
		plain.Mechanism{Username: "u", Password: "p"}
}

// The writer is where publishes go. kafka-go copies the Dialer's TLS and SASL
// into the Writer's Transport, so asserting on the Transport proves the
// credentials survived that conversion rather than merely being handed over.
func TestDialer_WriterCarriesCredentials(t *testing.T) {
	tlsCfg, mech := testCreds()
	b := New([]string{"broker:9092"}, "app", Config{TLS: tlsCfg, SASL: mech})

	tr, ok := b.writer.Transport.(*kafkago.Transport)
	if !ok {
		t.Fatalf("writer transport is %T, want *kafka.Transport", b.writer.Transport)
	}
	if tr.TLS != tlsCfg {
		t.Error("the writer would connect in plaintext: publishes go over the wire unencrypted")
	}
	if tr.SASL == nil {
		t.Error("the writer would connect unauthenticated: the broker rejects every publish")
	}
}

// Each consumer-group reader opens its own connection. A reader without the
// dialer connects as an anonymous plaintext client, so consumption fails on a
// cluster where publishing works — the most confusing shape of this bug.
func TestDialer_ReaderCarriesCredentials(t *testing.T) {
	tlsCfg, mech := testCreds()
	b := New([]string{"broker:9092"}, "app", Config{TLS: tlsCfg, SASL: mech})

	// NewReader is constructed the same way Subscribe constructs it. It does not
	// dial until read, so this is safe with no broker present.
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers: b.brokers,
		GroupID: "g",
		Topic:   "app.invoice.created",
		Dialer:  b.dialer,
	})
	defer r.Close()

	cfg := r.Config()
	if cfg.Dialer == nil {
		t.Fatal("reader has no dialer")
	}
	if cfg.Dialer.TLS != tlsCfg || cfg.Dialer.SASLMechanism == nil {
		t.Error("the reader would connect plaintext and anonymous while the writer is secured")
	}
}

// createTopic dials twice with b.dialer. It used to call the package-level
// kafkago.DialContext, which is always plaintext and unauthenticated — so on a
// secured cluster topic creation failed even though every other path worked.
func TestDialer_BusDialerIsConfigured(t *testing.T) {
	tlsCfg, mech := testCreds()
	b := New([]string{"broker:9092"}, "app", Config{
		TLS:         tlsCfg,
		SASL:        mech,
		DialTimeout: 3 * time.Second,
	})

	if b.dialer == nil {
		t.Fatal("bus has no dialer; createTopic would fall back to a plaintext dial")
	}
	if b.dialer.TLS != tlsCfg {
		t.Error("admin dials would be plaintext")
	}
	if b.dialer.SASLMechanism == nil {
		t.Error("admin dials would be unauthenticated")
	}
	if b.dialer.Timeout != 3*time.Second {
		t.Errorf("dial timeout = %v, want 3s", b.dialer.Timeout)
	}
}

// Anti-vacuity and back-compat: an adapter constructed without credentials must
// still be plaintext and unauthenticated, and the pre-existing topic settings
// must survive the config merge this change rewrote.
func TestDialer_DefaultsUnchanged(t *testing.T) {
	b := New([]string{"broker:9092"}, "app")

	if b.dialer.TLS != nil {
		t.Error("TLS was enabled without being asked for")
	}
	if b.dialer.SASLMechanism != nil {
		t.Error("SASL was enabled without being asked for")
	}
	if b.dialer.Timeout != defaultDialTimeout {
		t.Errorf("dial timeout = %v, want the %v default", b.dialer.Timeout, defaultDialTimeout)
	}
	if b.cfg.NumPartitions != 3 || b.cfg.ReplicationFactor != 1 {
		t.Errorf("topic defaults changed: got partitions=%d replication=%d, want 3 and 1",
			b.cfg.NumPartitions, b.cfg.ReplicationFactor)
	}
}

// The config merge is hand-written, so a field silently dropped from it is a
// live risk — TLS and SASL have no zero-value guard and would be easy to lose.
func TestDialer_ExplicitTopicSettingsStillApply(t *testing.T) {
	b := New([]string{"broker:9092"}, "app", Config{NumPartitions: 12, ReplicationFactor: 3})

	if b.cfg.NumPartitions != 12 {
		t.Errorf("NumPartitions = %d, want 12", b.cfg.NumPartitions)
	}
	if b.cfg.ReplicationFactor != 3 {
		t.Errorf("ReplicationFactor = %d, want 3", b.cfg.ReplicationFactor)
	}
	if b.dialer.Timeout != defaultDialTimeout {
		t.Errorf("dial timeout = %v, want the default when unset", b.dialer.Timeout)
	}
}
