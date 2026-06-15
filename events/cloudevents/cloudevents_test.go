package cloudevents_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/events"
	"github.com/xaleel/maniflex/events/cloudevents"
)

// fullEvent returns an Event with every field populated so round-trip tests
// can verify nothing is silently dropped.
func fullEvent() events.Event {
	return events.Event{
		ID:        "01HZTEST00000000000000001",
		Source:    "hospital/billing",
		Type:      "invoice.created",
		Subject:   "invoice/inv-42",
		Time:      time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		DataType:  "application/json",
		Data:      json.RawMessage(`{"amount":100}`),
		Model:     "Invoice",
		Operation: maniflex.OpCreate,
		RecordID:  "inv-42",
		ActorID:   "user-7",
		TenantID:  "tenant-9",
		TraceID:   "00-trace0000000000000000-span00000000-01",
		SchemaVer: 3,
	}
}

// ── structured mode ───────────────────────────────────────────────────────────

func TestStructuredEncode_Decode_RoundTrip(t *testing.T) {
	orig := fullEvent()
	b, err := cloudevents.Encode(orig)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := cloudevents.Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	assertEventsEqual(t, orig, got)
}

func TestStructuredDecode_RejectsWrongSpecVersion(t *testing.T) {
	body := []byte(`{"specversion":"0.3","id":"x","source":"s","type":"t","time":"2026-01-01T00:00:00Z"}`)
	if _, err := cloudevents.Decode(body); err == nil {
		t.Fatal("expected error for specversion 0.3, got nil")
	}
}

func TestStructuredDecode_RejectsMalformedJSON(t *testing.T) {
	if _, err := cloudevents.Decode([]byte(`not json`)); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestStructuredEncode_ContentType(t *testing.T) {
	b, err := cloudevents.Encode(fullEvent())
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env["specversion"]; !ok {
		t.Error("structured envelope missing specversion")
	}
}

// ── binary mode ───────────────────────────────────────────────────────────────

func TestBinaryEncode_Decode_RoundTrip(t *testing.T) {
	orig := fullEvent()
	h := make(http.Header)
	body := cloudevents.BinaryEncode(h, orig)
	got, err := cloudevents.BinaryDecode(h, body)
	if err != nil {
		t.Fatalf("BinaryDecode: %v", err)
	}

	assertEventsEqual(t, orig, got)
}

// TestBinaryRoundTrip_SchemaVer is a RED test for E6.
//
// BinaryEncode does not set ce-xschemaver and BinaryDecode does not read it,
// so SchemaVer is silently dropped in binary mode.
//
// Fix (E6): set ce-xschemaver in BinaryEncode when e.SchemaVer != 0; parse it
// back in BinaryDecode via strconv.Atoi(h.Get("ce-xschemaver")).
func TestBinaryRoundTrip_SchemaVer(t *testing.T) {
	orig := events.Event{
		ID:        "01HZTEST00000000000000002",
		Source:    "svc",
		Type:      "x.y",
		Time:      time.Now().UTC(),
		SchemaVer: 42,
	}
	h := make(http.Header)
	body := cloudevents.BinaryEncode(h, orig)
	got, err := cloudevents.BinaryDecode(h, body)
	if err != nil {
		t.Fatalf("BinaryDecode: %v", err)
	}
	if got.SchemaVer != 42 {
		t.Fatalf("SchemaVer lost in binary round-trip: got %d, want 42 (E6: BinaryEncode never sets ce-xschemaver)", got.SchemaVer)
	}
}

func TestBinaryDecode_RejectsWrongSpecVersion(t *testing.T) {
	h := make(http.Header)
	h.Set("ce-specversion", "0.3")
	if _, err := cloudevents.BinaryDecode(h, nil); err == nil {
		t.Fatal("expected error for ce-specversion 0.3, got nil")
	}
}

func TestBinaryEncode_SetsTraceparent(t *testing.T) {
	e := events.Event{
		ID:      "id1",
		Source:  "s",
		Type:    "t",
		Time:    time.Now(),
		TraceID: "00-aabbcc-ddeeff-01",
	}
	h := make(http.Header)
	cloudevents.BinaryEncode(h, e)
	if got := h.Get("traceparent"); got != e.TraceID {
		t.Fatalf("traceparent header: got %q, want %q", got, e.TraceID)
	}
}

// ── helper ────────────────────────────────────────────────────────────────────

func assertEventsEqual(t *testing.T, want, got events.Event) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("ID: got %q, want %q", got.ID, want.ID)
	}
	if got.Source != want.Source {
		t.Errorf("Source: got %q, want %q", got.Source, want.Source)
	}
	if got.Type != want.Type {
		t.Errorf("Type: got %q, want %q", got.Type, want.Type)
	}
	if got.Subject != want.Subject {
		t.Errorf("Subject: got %q, want %q", got.Subject, want.Subject)
	}
	if !got.Time.Equal(want.Time) {
		t.Errorf("Time: got %v, want %v", got.Time, want.Time)
	}
	if got.DataType != want.DataType {
		t.Errorf("DataType: got %q, want %q", got.DataType, want.DataType)
	}
	if string(got.Data) != string(want.Data) {
		t.Errorf("Data: got %q, want %q", got.Data, want.Data)
	}
	if got.Model != want.Model {
		t.Errorf("Model: got %q, want %q", got.Model, want.Model)
	}
	if got.Operation != want.Operation {
		t.Errorf("Operation: got %q, want %q", got.Operation, want.Operation)
	}
	if got.RecordID != want.RecordID {
		t.Errorf("RecordID: got %q, want %q", got.RecordID, want.RecordID)
	}
	if got.ActorID != want.ActorID {
		t.Errorf("ActorID: got %q, want %q", got.ActorID, want.ActorID)
	}
	if got.TenantID != want.TenantID {
		t.Errorf("TenantID: got %q, want %q", got.TenantID, want.TenantID)
	}
	if got.TraceID != want.TraceID {
		t.Errorf("TraceID: got %q, want %q", got.TraceID, want.TraceID)
	}
	if got.SchemaVer != want.SchemaVer {
		t.Errorf("SchemaVer: got %d, want %d", got.SchemaVer, want.SchemaVer)
	}
}
