// Package realtime provides a WebSocket and SSE hub backed by events.Bus.
package realtime

import (
	"fmt"
	"net/http"
	"strings"
)

// Principal represents an authenticated (or anonymous) hub connection.
type Principal struct {
	UserID   string
	TenantID string
	Roles    []string
	Scopes   []string
	Claims   map[string]any
}

// Authenticator validates an incoming connection and returns a Principal.
// Return (*Principal, nil) to allow the connection.
// Return (nil, err) to reject it — the hub sends 401 Unauthorized.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// ErrUnauthorized is returned by an Authenticator when a connection is rejected.
type ErrUnauthorized struct {
	Reason string
}

func (e *ErrUnauthorized) Error() string {
	return fmt.Sprintf("unauthorized: %s", e.Reason)
}

// AnonymousOnly allows every connection and returns an empty Principal.
// Suitable for public hubs where no authentication is needed.
type AnonymousOnly struct{}

func (AnonymousOnly) Authenticate(_ *http.Request) (*Principal, error) {
	return &Principal{}, nil
}

// bearerTokenAuth extracts a bearer token from the request and calls verify.
type bearerTokenAuth struct {
	verify func(token string) (*Principal, error)
}

// BearerToken returns an Authenticator that extracts a bearer token from:
//  1. Authorization: Bearer <token> header
//  2. ?access_token=<token> query parameter
//  3. Sec-WebSocket-Protocol: access_token.<token> header
//
// The token is passed to verify; its result is returned directly.
// If no token is found, ErrUnauthorized is returned without calling verify.
func BearerToken(verify func(token string) (*Principal, error)) Authenticator {
	return &bearerTokenAuth{verify: verify}
}

func (a *bearerTokenAuth) Authenticate(r *http.Request) (*Principal, error) {
	tok := extractBearerToken(r)
	if tok == "" {
		return nil, &ErrUnauthorized{Reason: "no bearer token"}
	}
	return a.verify(tok)
}

func extractBearerToken(r *http.Request) string {
	// 1. Authorization: Bearer <token>
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	// 2. ?access_token=<token>
	if tok := r.URL.Query().Get("access_token"); tok != "" {
		return tok
	}
	// 3. Sec-WebSocket-Protocol: access_token.<token>
	if proto := r.Header.Get("Sec-Websocket-Protocol"); proto != "" {
		if after, ok := strings.CutPrefix(proto, "access_token."); ok {
			return after
		}
	}
	return ""
}

// compositeAuth tries each Authenticator in order and returns the first success.
// If all fail, the last error is returned.
type compositeAuth struct {
	auths []Authenticator
}

// Composite returns an Authenticator that tries each provided authenticator in
// order and returns the result of the first one that succeeds. If all fail, it
// returns the last error.
func Composite(auths ...Authenticator) Authenticator {
	return &compositeAuth{auths: auths}
}

func (c *compositeAuth) Authenticate(r *http.Request) (*Principal, error) {
	var lastErr error
	for _, a := range c.auths {
		p, err := a.Authenticate(r)
		if err == nil {
			return p, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = &ErrUnauthorized{Reason: "no authenticators"}
	}
	return nil, lastErr
}
