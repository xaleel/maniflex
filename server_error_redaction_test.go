package maniflex

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerContextAbortRedactsAndLogsServerError(t *testing.T) {
	var logs bytes.Buffer
	ctx := &ServerContext{
		RequestID: "request-sec-02",
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
	}
	const privateError = `pq: relation "private_accounts" does not exist; password=db-secret`

	ctx.Abort(http.StatusInternalServerError, "DB_ERROR", privateError)

	if got := ctx.Response.Error.Message; got != "internal server error" {
		t.Fatalf("wire message = %q, want generic 500", got)
	}
	if got := logs.String(); !strings.Contains(got, "private_accounts") ||
		!strings.Contains(got, "db-secret") ||
		!strings.Contains(got, "request-sec-02") ||
		!strings.Contains(got, "DB_ERROR") {
		t.Fatalf("private diagnostic, request ID, and code must be logged; got:\n%s", got)
	}

	rec := httptest.NewRecorder()
	writeResponse(rec, ctx)
	if count := strings.Count(logs.String(), "db-secret"); count != 1 {
		t.Errorf("private diagnostic logged %d times, want once", count)
	}
	if strings.Contains(rec.Body.String(), "private_accounts") ||
		strings.Contains(rec.Body.String(), "db-secret") {
		t.Errorf("private diagnostic leaked to response: %s", rec.Body.String())
	}
}

func TestServerContextAbortPreservesValidatedClientError(t *testing.T) {
	var logs bytes.Buffer
	ctx := &ServerContext{
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
	}

	ctx.Abort(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "email is invalid")

	if got := ctx.Response.Error.Message; got != "email is invalid" {
		t.Fatalf("4xx message = %q, want original validated message", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("validated 4xx must not be logged as a server error:\n%s", logs.String())
	}
}

func TestWriteResponseRedactsDirectServerErrorAndDetails(t *testing.T) {
	var logs bytes.Buffer
	const privateError = "vault key payments-prod is sealed"
	ctx := &ServerContext{
		RequestID: "request-direct-500",
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{
			Level: slog.LevelError,
		})),
		Response: &APIResponse{
			StatusCode: http.StatusBadGateway,
			Error: &APIError{
				Code:    "KEY_PROVIDER_ERROR",
				Message: privateError,
				Details: map[string]any{"vault_path": "secret/payments"},
			},
		},
	}

	rec := httptest.NewRecorder()
	writeResponse(rec, ctx)

	var body struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body.String())
	}
	if body.Error.Code != "KEY_PROVIDER_ERROR" {
		t.Errorf("code = %q, want KEY_PROVIDER_ERROR", body.Error.Code)
	}
	if body.Error.Message != "bad gateway" {
		t.Errorf("message = %q, want generic 502", body.Error.Message)
	}
	if body.Error.Details != nil {
		t.Errorf("5xx details leaked: %#v", body.Error.Details)
	}
	if got := logs.String(); !strings.Contains(got, privateError) ||
		!strings.Contains(got, "request-direct-500") {
		t.Errorf("private diagnostic and request ID must be logged; got:\n%s", got)
	}
}

func TestAPIResponseWriteRedactsAsIsServerBody(t *testing.T) {
	rec := httptest.NewRecorder()
	resp := &APIResponse{
		StatusCode: http.StatusInternalServerError,
		AsIs:       true,
		Data:       json.RawMessage(`{"dsn":"postgres://admin:secret@db/private"}`),
	}

	resp.Write(rec)

	if strings.Contains(rec.Body.String(), "postgres") ||
		strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("AsIs 5xx body leaked: %s", rec.Body.String())
	}
	var body struct {
		Error APIError `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error.Code != "INTERNAL" || body.Error.Message != "internal server error" {
		t.Errorf("error = %#v, want generic INTERNAL", body.Error)
	}
}

func TestWriteJSONErrorRedactsServerMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusInternalServerError, "STORE_ERROR",
		`bucket private-prod: access key AKIA-EXAMPLE rejected`)

	if strings.Contains(rec.Body.String(), "private-prod") ||
		strings.Contains(rec.Body.String(), "AKIA-EXAMPLE") {
		t.Errorf("standalone JSON error leaked: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"message":"internal server error"`) {
		t.Errorf("unexpected generic response: %s", rec.Body.String())
	}
}
