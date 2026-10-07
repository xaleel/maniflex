package auth

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/xaleel/maniflex"
)

// ReadAuditRecord is the structured log entry written for every audited read
// or list operation. It is intentionally richer than the write AuditRecord
// because read access to clinical/financial data is the primary HIPAA-equivalent
// control surface.
type ReadAuditRecord struct {
	Timestamp   time.Time          `json:"timestamp"`
	Model       string             `json:"model"`
	Operation   maniflex.Operation `json:"operation"`
	RecordID    string             `json:"record_id,omitempty"`    // OpRead: the accessed record's ID
	RecordCount int                `json:"record_count,omitempty"` // OpList: number of records returned
	Actor       string             `json:"actor,omitempty"`
	Roles       []string           `json:"roles,omitempty"`
	TenantID    string             `json:"tenant_id,omitempty"`
	SessionID   string             `json:"session_id,omitempty"`
	AuthMethod  string             `json:"auth_method,omitempty"`
	RequestID   string             `json:"request_id,omitempty"`
	TraceID     string             `json:"trace_id,omitempty"`
	ServiceName string             `json:"service_name,omitempty"`
	IPAddress   string             `json:"ip_address,omitempty"`
	UserAgent   string             `json:"user_agent,omitempty"`
}

// ReadAuditSink receives read audit records. Implement this interface to write
// to a database table, structured logger, or external audit service.
//
// The canonical implementation stores records in an append-only
// read_audit_log table with no UPDATE or DELETE privileges on the DB role.
type ReadAuditSink interface {
	Write(ctx context.Context, record ReadAuditRecord) error
}

// readAuditWriteTimeout bounds one sink write. It matches db.AuditLog's, so the
// two audit writers in the tree behave the same way under a slow sink.
const readAuditWriteTimeout = 5 * time.Second

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
// Audit writes are fire-and-forget, so a slow sink never delays the HTTP
// response: the record is built synchronously and handed to ctx.GoBackground
// with a 5 s write timeout. A sink error is logged at Warn rather than
// discarded, and a panicking sink is recovered and reported rather than taking
// the process down with it.
//
// Fire-and-forget bounds the loss rather than removing it. Server.Shutdown
// waits for audit writes in flight, so a record is no longer abandoned by a
// rolling deploy, but that wait is itself bounded by Config.ShutdownTimeout —
// past it the write is cancelled and the record is gone, and the shutdown log
// says how many did not finish. A sink that must not lose anything should write
// to a reliable queue rather than to its final destination.
//
// One goroutine per audited read is in flight until its write returns, so a
// read burst against a slow sink costs roughly rate × 5 s goroutines. That is
// the cost of not delaying responses; a sink that honours the context it is
// given keeps it bounded.
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

		// GoBackground, not a bare goroutine. Server.Shutdown waits for tracked
		// goroutines, so the reads served in the last moments of a replica are
		// still audited instead of being abandoned mid-write on every rolling
		// deploy — the loss GoBackground was added to prevent, and the one thing
		// this type's own doc calls the primary control surface. It also carries
		// the framework's panic recovery, so a sink that panics (a DB handle
		// closed during shutdown, an encoder bug) is reported through
		// Config.PanicLogger and Config.OnBackgroundPanic rather than unwinding
		// into the runtime and killing the process: chi's Recoverer only covers
		// the request goroutine, which has already returned by here (audit
		// AUTH-13).
		ctx.GoBackground(func(bgCtx context.Context) {
			writeCtx, cancel := context.WithTimeout(bgCtx, readAuditWriteTimeout)
			defer cancel()
			if err := sink.Write(writeCtx, record); err != nil {
				ctx.Logger().Warn("auth: read audit sink write failed",
					"model", record.Model,
					"operation", string(record.Operation),
					"request_id", record.RequestID,
					"error", err.Error())
			}
		})

		return nil
	}
}

// extractIP returns the client IP from the request's RemoteAddr. When the
// server trusts proxy headers (Config.TrustProxyHeaders), the router has already
// rewritten RemoteAddr from X-Forwarded-For / X-Real-IP,
// so the resolved client IP flows through here; otherwise RemoteAddr is the
// direct TCP peer. extractIP deliberately does NOT read the forwarding headers
// itself — trusting a client-supplied address even when proxy headers are
// untrusted would let audit logs record forged IPs (SEC-5).
func extractIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
