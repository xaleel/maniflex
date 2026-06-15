// Package events provides a CloudEvents 1.0-aligned event bus for maniflex.
//
// The core package is stdlib-only. Broker adapters live in sub-packages:
//   - events/inproc  — in-process fan-out (tests, single-binary apps)
//   - events/outbox  — transactional outbox over *sql.DB
//   - events/redis   — Redis Streams (XADD / XREADGROUP)
//   - events/nats    — NATS JetStream
//   - events/kafka   — kafka-go
//   - events/rabbitmq — AMQP 0.9.1 (RabbitMQ)
//   - events/cloudevents — CloudEvents 1.0 binary/structured codec
package events

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"maniflex"
)

// Event is a CloudEvents 1.0-aligned event envelope with maniflex extensions.
// CloudEvents 1.0 core attributes are first-class fields; maniflex-specific
// attributes ride as CE extension attributes (lowercase, no underscores per the CE spec).
type Event struct {
	// CloudEvents 1.0 core attributes
	ID       string          `json:"id"`
	Source   string          `json:"source"`
	Type     string          `json:"type"`
	Subject  string          `json:"subject,omitempty"`
	Time     time.Time       `json:"time"`
	DataType string          `json:"datatype,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`

	// maniflex extension attributes
	Model     string        `json:"model,omitempty"`
	Operation maniflex.Operation `json:"operation,omitempty"`
	RecordID  string        `json:"recordid,omitempty"`
	ActorID   string        `json:"actorid,omitempty"`
	TenantID  string        `json:"tenantid,omitempty"`
	TraceID   string        `json:"traceid,omitempty"`
	SchemaVer int           `json:"schemaver,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// Publisher is the producer-only interface. Publish-only backends (SNS,
// transactional outbox) implement this without implementing Subscribe.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
	PublishBatch(ctx context.Context, es []Event) error
	Close() error
}

// Bus extends Publisher with the consumer-side Subscribe method.
// Adapters that support both directions (inproc, Redis, NATS, Kafka) implement Bus.
type Bus interface {
	Publisher
	Subscribe(ctx context.Context, sub Subscription) (Cancel, error)
}

// Cancel stops a subscription. Safe to call multiple times.
type Cancel func()

// AckMode controls how events are acknowledged.
type AckMode int

const (
	// AutoAck acknowledges automatically when the handler returns nil.
	// A non-nil return triggers retry (up to Subscription.MaxRetry times).
	AutoAck AckMode = iota
	// ManualAck is reserved for a future extension.
	ManualAck
)

// Handler processes a single event. Return nil to ack; non-nil to nack and retry.
type Handler func(ctx context.Context, e Event) error

// Subscription describes which events to consume and how to process them.
type Subscription struct {
	// Group is the consumer-group name for Kafka, JetStream, and Redis XREADGROUP.
	// Ignored by in-process subscriptions.
	Group string

	// Patterns is a list of glob-style event Type patterns.
	// "invoice.*" matches "invoice.created", "invoice.updated", etc.
	// Translated to broker-native filters where supported.
	Patterns []string

	// Handler is called for each matching event.
	Handler Handler

	// Concurrency is the number of concurrent handler goroutines. Default: 1.
	Concurrency int

	// AckMode controls acknowledgement. Default: AutoAck.
	AckMode AckMode

	// MaxRetry is the number of retries before sending to the DLQ. Default: 3.
	MaxRetry int

	// Backoff returns the pre-retry delay. Default: linear (attempt × second).
	Backoff func(attempt int) time.Duration

	// DLQ is the event Type published after MaxRetry exhaustion.
	// Empty string disables dead-lettering.
	DLQ string
}

// TxPublisher is an optional interface implemented by outbox.Bus.
// When the active Publisher implements TxPublisher and a database transaction
// is in flight, Emit calls PublishWithExecer so the outbox INSERT commits
// atomically with the business write.
type TxPublisher interface {
	Publisher
	PublishWithExecer(ctx context.Context, ex SQLExecer, e Event) error
}

// SQLExecer is satisfied by both *sql.DB and *sql.Tx.
// Used by TxPublisher.PublishWithExecer to INSERT outbox rows within an
// existing database transaction without holding a direct *sql.Tx reference.
type SQLExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// DeliverWithRetry calls sub.Handler up to sub.MaxRetry+1 times with
// exponential backoff. If all attempts fail and sub.DLQ is non-empty, the
// event is re-published under the DLQ type via pub so it flows through the
// same broker as the original event.
//
// Roadmap §11B.11 / checkpoint H11: the retry sleep honours ctx
// cancellation so shutdown doesn't leave handler goroutines blocked on a
// long backoff. The DLQ event gets a fresh ID (downstream dedupers no
// longer see it as a duplicate of the original) and publish errors are
// logged rather than silently swallowed.
func DeliverWithRetry(ctx context.Context, pub Publisher, sub Subscription, e Event) {
	for attempt := 0; attempt <= sub.MaxRetry; attempt++ {
		if err := sub.Handler(ctx, e); err == nil {
			return
		}
		if attempt < sub.MaxRetry && sub.Backoff != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(sub.Backoff(attempt + 1)):
			}
		}
	}
	if sub.DLQ != "" {
		dlq := e
		dlq.ID = newID() // fresh ID so downstream dedupers see this as a new event
		dlq.Type = sub.DLQ
		newHeaders := make(map[string]string, len(e.Headers)+1)
		for k, v := range e.Headers {
			newHeaders[k] = v
		}
		newHeaders["original_type"] = e.Type
		newHeaders["original_id"] = e.ID
		dlq.Headers = newHeaders
		if err := pub.Publish(ctx, dlq); err != nil {
			slog.Default().Error("events: DLQ publish failed",
				slog.String("dlq_type", sub.DLQ),
				slog.String("original_id", e.ID),
				slog.String("error", err.Error()))
		}
	}
}

// newID returns a ULID-format identifier: 48-bit millisecond timestamp followed
// by 80 random bits, encoded as 26 Crockford base32 characters.
// Pure stdlib, no third-party dependencies.
func newID() string {
	ms := time.Now().UnixMilli()
	var b [16]byte
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := io.ReadFull(rand.Reader, b[6:]); err != nil {
		panic("events: entropy read: " + err.Error())
	}
	return crockfordEncode(b)
}

const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// crockfordEncode encodes 16 bytes (128 bits) as 26 Crockford base32 characters.
// The final group covers 3 bits from byte 15 (128 bits used, 130 encoded, 2 zero-padded).
func crockfordEncode(b [16]byte) string {
	dst := make([]byte, 26)
	for i := 0; i < 26; i++ {
		bit := i * 5
		byteIdx := bit / 8
		bitOff := uint(bit % 8)
		var v byte
		if bitOff <= 3 {
			v = (b[byteIdx] >> (3 - bitOff)) & 0x1f
		} else {
			hi := b[byteIdx] << (bitOff - 3)
			var lo byte
			if byteIdx+1 < 16 {
				lo = b[byteIdx+1] >> (11 - bitOff)
			}
			v = (hi | lo) & 0x1f
		}
		dst[i] = crockfordAlphabet[v]
	}
	return string(dst)
}
