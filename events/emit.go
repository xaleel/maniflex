package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"maniflex"
)

// TypeFunc derives the CloudEvents Type from a Server pipeline context.
type TypeFunc func(ctx *maniflex.ServerContext) string

// SubjectFunc derives the CloudEvents Subject from a Server pipeline context.
type SubjectFunc func(ctx *maniflex.ServerContext) string

// EmitConfig customises the Event fields produced by the Emit middleware.
// All fields are optional; unset fields fall back to sensible defaults.
type EmitConfig struct {
	// Source overrides ctx.ServiceName() when non-empty.
	Source string
	// Type derives the event Type field. Default: DefaultType.
	Type TypeFunc
	// Subject derives the event Subject field. Default: DefaultSubject.
	Subject SubjectFunc
	// TenantID extracts the tenant partition key from the context.
	// Default: reads ctx.Auth.Claims["tenant_id"] when present.
	TenantID func(ctx *maniflex.ServerContext) string
	// SchemaVer is stamped on every produced event. Default: 0 (unversioned).
	SchemaVer int
}

// DefaultType is the default TypeFunc: returns "{model}.{operation}" in lowercase,
// e.g. "invoice.created", "user.deleted".
var DefaultType TypeFunc = func(ctx *maniflex.ServerContext) string {
	suffix := map[maniflex.Operation]string{
		maniflex.OpCreate: "created",
		maniflex.OpUpdate: "updated",
		maniflex.OpDelete: "deleted",
		maniflex.OpRead:   "read",
		maniflex.OpList:   "listed",
	}
	s, ok := suffix[ctx.Operation]
	if !ok {
		s = string(ctx.Operation)
	}
	return lowerFirst(ctx.Model.Name) + "." + s
}

// DefaultSubject is the default SubjectFunc: returns "{model}/{record_id}",
// e.g. "invoice/abc123". Falls back to "{model}" when no resource ID is set.
var DefaultSubject SubjectFunc = func(ctx *maniflex.ServerContext) string {
	if ctx.ResourceID != "" {
		return lowerFirst(ctx.Model.Name) + "/" + ctx.ResourceID
	}
	return lowerFirst(ctx.Model.Name)
}

// Emit returns a maniflex.MiddlewareFunc that builds an Event from ServerContext and
// publishes it to pub after the DB step succeeds. Register it with
// AtPosition(After) on the DB pipeline step:
//
//	server.Pipeline.DB.Register(
//	    events.Emit(bus, events.EmitConfig{Source: cfg.ServiceName}),
//	    maniflex.ForOperation(maniflex.OpCreate, maniflex.OpUpdate, maniflex.OpDelete),
//	    maniflex.AtPosition(maniflex.After),
//	)
//
// Transactional outbox: when pub implements TxPublisher and ctx.Tx is non-nil,
// the event INSERT commits atomically with the business write.
// Otherwise the publish runs fire-and-forget in a background goroutine.
func Emit(pub Publisher, cfgs ...EmitConfig) maniflex.MiddlewareFunc {
	cfg := EmitConfig{}
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	if cfg.Type == nil {
		cfg.Type = DefaultType
	}
	if cfg.Subject == nil {
		cfg.Subject = DefaultSubject
	}

	return func(ctx *maniflex.ServerContext, next func() error) error {
		if err := next(); err != nil {
			return err
		}
		if ctx.Response != nil && ctx.Response.StatusCode >= 400 {
			return nil
		}

		source := cfg.Source
		if source == "" {
			source = ctx.ServiceName()
		}

		actorID := ""
		if ctx.Auth != nil {
			actorID = ctx.Auth.UserID
		}

		tenantID := ""
		if cfg.TenantID != nil {
			tenantID = cfg.TenantID(ctx)
		} else if ctx.Auth != nil {
			if ctx.Auth.TenantID != "" {
				tenantID = ctx.Auth.TenantID
			} else if v, ok := ctx.Auth.Claims["tenant_id"].(string); ok {
				tenantID = v
			}
		}

		data, _ := json.Marshal(ctx.DBResult)

		e := Event{
			ID:        newID(),
			Source:    source,
			Type:      cfg.Type(ctx),
			Subject:   cfg.Subject(ctx),
			Time:      time.Now().UTC(),
			DataType:  "application/json",
			Data:      json.RawMessage(data),
			Model:     ctx.Model.Name,
			Operation: ctx.Operation,
			RecordID:  ctx.ResourceID,
			ActorID:   actorID,
			TenantID:  tenantID,
			TraceID:   ctx.TraceID,
			SchemaVer: cfg.SchemaVer,
		}

		// Transactional outbox: INSERT within the active business transaction.
		if txp, ok := pub.(TxPublisher); ok && ctx.Tx != nil {
			ex, ok := ctx.Tx.(SQLExecer)
			if !ok {
				return fmt.Errorf("events: ctx.Tx (%T) does not implement SQLExecer; outbox durability lost", ctx.Tx)
			}
			return txp.PublishWithExecer(ctx.Ctx, ex, e)
		}

		// Non-transactional: fire-and-forget in a goroutine.
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = pub.Publish(bgCtx, e)
		}()
		return nil
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 32
	}
	return string(b)
}
