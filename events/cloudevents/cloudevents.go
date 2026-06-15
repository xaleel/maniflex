// Package cloudevents provides encoding and decoding helpers for CloudEvents 1.0.
// It converts events.Event to/from the CloudEvents structured-content-mode JSON
// envelope and the binary-content-mode HTTP representation.
//
// Pure stdlib — no third-party dependencies.
package cloudevents

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"maniflex"
	"maniflex/events"
)

const specVersion = "1.0"

// StructuredEnvelope is the CloudEvents 1.0 structured-content-mode JSON body.
// The Content-Type of the HTTP body must be "application/cloudevents+json".
type StructuredEnvelope struct {
	// CloudEvents 1.0 core attributes
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject,omitempty"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`

	// maniflex extension attributes (prefixed with "x" per CE spec)
	Xmodel     string `json:"xmodel,omitempty"`
	Xoperation string `json:"xoperation,omitempty"`
	Xrecordid  string `json:"xrecordid,omitempty"`
	Xactorid   string `json:"xactorid,omitempty"`
	Xtenantid  string `json:"xtenantid,omitempty"`
	Xtraceid   string `json:"xtraceid,omitempty"`
	Xschemaver int    `json:"xschemaver,omitempty"`
}

// Encode serialises e as a CloudEvents 1.0 structured-content-mode JSON envelope.
func Encode(e events.Event) ([]byte, error) {
	env := StructuredEnvelope{
		SpecVersion:     specVersion,
		ID:              e.ID,
		Source:          e.Source,
		Type:            e.Type,
		Subject:         e.Subject,
		Time:            e.Time,
		DataContentType: e.DataType,
		Data:            e.Data,
		Xmodel:          e.Model,
		Xoperation:      string(e.Operation),
		Xrecordid:       e.RecordID,
		Xactorid:        e.ActorID,
		Xtenantid:       e.TenantID,
		Xtraceid:        e.TraceID,
		Xschemaver:      e.SchemaVer,
	}
	return json.Marshal(env)
}

// Decode parses a CloudEvents 1.0 structured-content-mode JSON body.
// Returns an error when specversion is absent or not "1.0".
func Decode(body []byte) (events.Event, error) {
	var env StructuredEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return events.Event{}, fmt.Errorf("cloudevents: decode: %w", err)
	}
	if env.SpecVersion != specVersion {
		return events.Event{}, fmt.Errorf("cloudevents: unsupported specversion %q", env.SpecVersion)
	}
	return events.Event{
		ID:        env.ID,
		Source:    env.Source,
		Type:      env.Type,
		Subject:   env.Subject,
		Time:      env.Time,
		DataType:  env.DataContentType,
		Data:      env.Data,
		Model:     env.Xmodel,
		Operation: maniflex.Operation(env.Xoperation),
		RecordID:  env.Xrecordid,
		ActorID:   env.Xactorid,
		TenantID:  env.Xtenantid,
		TraceID:   env.Xtraceid,
		SchemaVer: env.Xschemaver,
	}, nil
}

// ContentType is the HTTP Content-Type for structured-mode CloudEvents.
const ContentType = "application/cloudevents+json"

// BinaryEncode sets CloudEvents 1.0 binary-content-mode headers on h and returns
// the data bytes for the request body. Set the Content-Type header separately
// using e.DataType (usually "application/json").
func BinaryEncode(h http.Header, e events.Event) []byte {
	h.Set("ce-specversion", specVersion)
	h.Set("ce-id", e.ID)
	h.Set("ce-source", e.Source)
	h.Set("ce-type", e.Type)
	h.Set("ce-time", e.Time.UTC().Format(time.RFC3339Nano))
	if e.Subject != "" {
		h.Set("ce-subject", e.Subject)
	}
	if e.DataType != "" {
		h.Set("Content-Type", e.DataType)
	}
	if e.Model != "" {
		h.Set("ce-xmodel", e.Model)
	}
	if e.Operation != "" {
		h.Set("ce-xoperation", string(e.Operation))
	}
	if e.RecordID != "" {
		h.Set("ce-xrecordid", e.RecordID)
	}
	if e.ActorID != "" {
		h.Set("ce-xactorid", e.ActorID)
	}
	if e.TenantID != "" {
		h.Set("ce-xtenantid", e.TenantID)
	}
	if e.TraceID != "" {
		h.Set("ce-xtraceid", e.TraceID)
		h.Set("traceparent", e.TraceID) // also set W3C traceparent for compatibility
	}
	if e.SchemaVer != 0 {
		h.Set("ce-xschemaver", strconv.Itoa(e.SchemaVer))
	}
	return e.Data
}

// BinaryDecode reconstructs an Event from CloudEvents 1.0 binary-content-mode
// HTTP headers and the request body.
func BinaryDecode(h http.Header, body []byte) (events.Event, error) {
	if sv := h.Get("ce-specversion"); sv != specVersion {
		return events.Event{}, fmt.Errorf("cloudevents: unsupported ce-specversion %q", sv)
	}
	var t time.Time
	if ts := h.Get("ce-time"); ts != "" {
		var err error
		t, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			t, _ = time.Parse(time.RFC3339, ts)
		}
	}
	schemaVer, _ := strconv.Atoi(h.Get("ce-xschemaver"))
	return events.Event{
		ID:        h.Get("ce-id"),
		Source:    h.Get("ce-source"),
		Type:      h.Get("ce-type"),
		Subject:   h.Get("ce-subject"),
		Time:      t,
		DataType:  h.Get("Content-Type"),
		Data:      json.RawMessage(body),
		Model:     h.Get("ce-xmodel"),
		Operation: maniflex.Operation(h.Get("ce-xoperation")),
		RecordID:  h.Get("ce-xrecordid"),
		ActorID:   h.Get("ce-xactorid"),
		TenantID:  h.Get("ce-xtenantid"),
		TraceID:   h.Get("ce-xtraceid"),
		SchemaVer: schemaVer,
	}, nil
}
