package auth

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/xaleel/maniflex"
)

// ReadAuditRecord is the structured log entry written for every audited read
// or list operation. It is intentionally richer than the write AuditRecord
// because read access to clinical/financial data is the primary HIPAA-equivalent
// control surface.
type ReadAuditRecord struct {
	Timestamp   time.Time     `json:"timestamp"`
	Model       string        `json:"model"`
	Operation   maniflex.Operation `json:"operation"`
	RecordID    string        `json:"record_id,omitempty"`    // OpRead: the accessed record's ID
	RecordCount int           `json:"record_count,omitempty"` // OpList: number of records returned
	Actor       string        `json:"actor,omitempty"`
	Roles       []string      `json:"roles,omitempty"`
	TenantID    string        `json:"tenant_id,omitempty"`
	SessionID   string        `json:"session_id,omitempty"`
	AuthMethod  string        `json:"auth_method,omitempty"`
	RequestID   string        `json:"request_id,omitempty"`
	TraceID     string        `json:"trace_id,omitempty"`
	ServiceName string        `json:"service_name,omitempty"`
	IPAddress   string        `json:"ip_address,omitempty"`
	UserAgent   string        `json:"user_agent,omitempty"`
}

// ReadAuditSink receives read audit records. Implement this interface to write
// to a database table, structured logger, or external audit service.
//
// The canonical implementation stores records in an append-only
// read_audit_log table with no UPDATE or DELETE privileges on the DB role.
type ReadAuditSink interface {
	Write(ctx context.Context, record ReadAuditRecord) error
}

// ReadAudit writes a structured read-audit record after every successful read
// or list response. Register it with AtPosition(After) on the Response step so
// it fires only when the pipeline returns a 2xx.
//
//	server.Pipeline.Response.Register(
//	    auth.ReadAudit(myAuditSink),
//	    maniflex.ForModel("Patient", "ClinicalNote", "Prescription", "LabResult"),
//	    maniflex.ForOperation(maniflex.OpRead, maniflex.OpList),
//	    maniflex.AtPosition(maniflex.After),
//	)
//
// Audit writes are fire-and-forget (goroutine + 5 s timeout) so a slow sink
// never delays the HTTP response. Sink errors are silently discarded — use a
// sink that writes to a reliable queue if loss is unacceptable.
func ReadAudit(sink ReadAuditSink) maniflex.MiddlewareFunc {
	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		// Only audit successful responses (2xx).
		if ctx.Response == nil || ctx.Response.StatusCode >= 400 {
			return nil
		}

		record := ReadAuditRecord{
			Timestamp:   time.Now().UTC(),
			Model:       ctx.Model.Name,
			Operation:   ctx.Operation,
			RequestID:   ctx.RequestID,
			TraceID:     ctx.TraceID,
			ServiceName: ctx.ServiceName(),
			IPAddress:   extractIP(ctx.Request),
			UserAgent:   ctx.Request.Header.Get("User-Agent"),
		}

		if ctx.Auth != nil {
			record.Actor = ctx.Auth.UserID
			record.Roles = ctx.Auth.Roles
			record.TenantID = ctx.Auth.TenantID
			record.SessionID = ctx.Auth.SessionID
			record.AuthMethod = ctx.Auth.AuthMethod
		}

		switch ctx.Operation {
		case maniflex.OpRead:
			record.RecordID = ctx.ResourceID
		case maniflex.OpList:
			if lr, ok := ctx.DBResult.(*maniflex.ListResult); ok {
				record.RecordCount = len(lr.Items)
			}
		}

		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = sink.Write(bgCtx, record)
		}()

		return nil
	}
}

// extractIP returns the client IP from the request. Chi's RealIP middleware
// rewrites RemoteAddr from X-Real-IP / X-Forwarded-For when present, so
// checking RemoteAddr is usually sufficient. The header checks handle the
// case where RealIP is not in the middleware stack.
func extractIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
