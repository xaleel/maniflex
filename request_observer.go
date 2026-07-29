package maniflex

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
)

// RequestObservation is the completed view of one HTTP request.
//
// Duration spans router entry through response writing. Model and Operation are
// populated for generated model routes, actions, search, and attachment routes;
// they remain empty for protocol-only routes such as health and static files.
type RequestObservation struct {
	StartedAt time.Time
	Duration  time.Duration

	Method       string
	Path         string
	RoutePattern string
	Model        string
	Operation    Operation
	Status       int

	RequestID  string
	TraceID    string
	Actor      string
	ResourceID string
}

// RequestObserver receives one immutable observation after an HTTP request has
// finished. Observers run synchronously and should return quickly. A panic in an
// observer is recovered and logged so telemetry cannot corrupt a response that
// has already been written.
type RequestObserver func(RequestObservation)

type requestObservationState struct {
	serverContext *ServerContext
}

type requestObservationKey struct{}

func requestObservationMiddleware(logger *slog.Logger, observers []RequestObserver) HTTPMiddleware {
	registered := append([]RequestObserver(nil), observers...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			state := &requestObservationState{}
			r = r.WithContext(context.WithValue(r.Context(), requestObservationKey{}, state))
			wrapped := chiMiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				panicValue := recover()
				status := wrapped.Status()
				if panicValue != nil && status == 0 {
					status = http.StatusInternalServerError
				}
				if status == 0 {
					status = http.StatusOK
				}

				observation := RequestObservation{
					StartedAt:    started,
					Duration:     time.Since(started),
					Method:       r.Method,
					Path:         r.URL.Path,
					RoutePattern: chi.RouteContext(r.Context()).RoutePattern(),
					Status:       status,
					RequestID:    chiMiddleware.GetReqID(r.Context()),
				}
				if ctx := state.serverContext; ctx != nil {
					if ctx.Model != nil {
						observation.Model = ctx.Model.Name
					}
					observation.Operation = ctx.Operation
					observation.TraceID = ctx.TraceID
					observation.ResourceID = ctx.ResourceID
					if ctx.Auth != nil {
						observation.Actor = ctx.Auth.UserID
					}
				}

				for _, observer := range registered {
					notifyRequestObserver(logger, observer, observation)
				}
				if panicValue != nil {
					panic(panicValue)
				}
			}()

			next.ServeHTTP(wrapped, r)
		})
	}
}

func notifyRequestObserver(logger *slog.Logger, observer RequestObserver, observation RequestObservation) {
	defer func() {
		if panicValue := recover(); panicValue != nil {
			logger.Error("request observer panicked",
				slog.Any("panic", panicValue),
				slog.String("request_id", observation.RequestID))
		}
	}()
	observer(observation)
}

func attachRequestObservation(r *http.Request, ctx *ServerContext) {
	if r == nil {
		return
	}
	state, _ := r.Context().Value(requestObservationKey{}).(*requestObservationState)
	if state != nil {
		state.serverContext = ctx
	}
}
