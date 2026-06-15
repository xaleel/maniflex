package realtime_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"maniflex/events"
	"maniflex/events/inproc"
	"maniflex/realtime"
)

// ── AnonymousOnly ─────────────────────────────────────────────────────────────

func TestAnonymousOnly_AlwaysReturnsEmptyPrincipal(t *testing.T) {
	t.Parallel()
	a := realtime.AnonymousOnly{}

	tests := []struct {
		name string
		req  *http.Request
	}{
		{"no headers", makeReq(t, nil)},
		{"with auth header", makeReq(t, map[string]string{"Authorization": "Bearer tok"})},
		{"with query param", makeReqWithQuery(t, "access_token=tok")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := a.Authenticate(tt.req)
			if err != nil {
				t.Fatalf("AnonymousOnly.Authenticate returned error: %v", err)
			}
			if p == nil {
				t.Fatal("AnonymousOnly must return non-nil Principal")
			}
			if p.UserID != "" {
				t.Errorf("AnonymousOnly.UserID must be empty, got %q", p.UserID)
			}
			if len(p.Roles) != 0 {
				t.Errorf("AnonymousOnly.Roles must be empty, got %v", p.Roles)
			}
		})
	}
}

// ── BearerToken ───────────────────────────────────────────────────────────────

func TestBearerToken_ExtractsFromAuthorizationHeader(t *testing.T) {
	t.Parallel()
	var receivedToken string
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		receivedToken = tok
		return &realtime.Principal{UserID: "u1"}, nil
	})

	req := makeReq(t, map[string]string{"Authorization": "Bearer mytoken123"})
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.UserID != "u1" {
		t.Errorf("UserID: got %q, want u1", p.UserID)
	}
	if receivedToken != "mytoken123" {
		t.Errorf("token passed to verify: got %q, want mytoken123", receivedToken)
	}
}

func TestBearerToken_ExtractsFromQueryParam_access_token(t *testing.T) {
	t.Parallel()
	var receivedToken string
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		receivedToken = tok
		return &realtime.Principal{UserID: "u2"}, nil
	})

	req := makeReqWithQuery(t, "access_token=querytoken456")
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.UserID != "u2" {
		t.Errorf("UserID: got %q, want u2", p.UserID)
	}
	if receivedToken != "querytoken456" {
		t.Errorf("token: got %q, want querytoken456", receivedToken)
	}
}

func TestBearerToken_ExtractsFromSecWebSocketProtocol(t *testing.T) {
	// Browsers cannot set custom headers on WebSocket(), so the standard
	// workaround is to send the token in the Sec-WebSocket-Protocol subprotocol
	// using the format "access_token.<token>".
	t.Parallel()
	var receivedToken string
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		receivedToken = tok
		return &realtime.Principal{UserID: "u3"}, nil
	})

	req := makeReq(t, map[string]string{
		"Sec-WebSocket-Protocol": "access_token.subprotocol789",
	})
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.UserID != "u3" {
		t.Errorf("UserID: got %q, want u3", p.UserID)
	}
	if receivedToken != "subprotocol789" {
		t.Errorf("token from Sec-WebSocket-Protocol: got %q, want subprotocol789", receivedToken)
	}
}

func TestBearerToken_AuthHeaderTakesPriorityOverQueryParam(t *testing.T) {
	// When both Authorization header and access_token query param are present,
	// the header takes priority.
	t.Parallel()
	var receivedToken string
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		receivedToken = tok
		return &realtime.Principal{UserID: "u1"}, nil
	})

	req, _ := http.NewRequest(http.MethodGet, "http://localhost/ws?access_token=query-token", nil)
	req.Header.Set("Authorization", "Bearer header-token")
	if _, err := a.Authenticate(req); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if receivedToken != "header-token" {
		t.Errorf("expected header-token to take priority, got %q", receivedToken)
	}
}

func TestBearerToken_NoToken_ReturnsError(t *testing.T) {
	t.Parallel()
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		return &realtime.Principal{}, nil
	})

	req := makeReq(t, nil)
	_, err := a.Authenticate(req)
	if err == nil {
		t.Fatal("expected error when no token present, got nil")
	}

	var unauth *realtime.ErrUnauthorized
	if !errors.As(err, &unauth) {
		t.Errorf("expected *ErrUnauthorized, got %T: %v", err, err)
	}
}

func TestBearerToken_VerifyErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("invalid signature")
	a := realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
		return nil, sentinel
	})

	req := makeReq(t, map[string]string{"Authorization": "Bearer badtoken"})
	_, err := a.Authenticate(req)
	if err == nil {
		t.Fatal("expected error from verifier to propagate, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error to propagate, got: %v", err)
	}
}

func TestBearerToken_WithJWT_ValidToken(t *testing.T) {
	t.Parallel()
	const secret = "realtime-test-secret"
	tok := makeTestJWT(secret, map[string]any{
		"sub":   "dr-jones",
		"roles": []string{"doctor"},
		"exp":   99999999999,
		"iat":   1000000000,
	})

	a := realtime.BearerToken(func(token string) (*realtime.Principal, error) {
		if token != tok {
			return nil, &realtime.ErrUnauthorized{Reason: "wrong token"}
		}
		return &realtime.Principal{
			UserID: "dr-jones",
			Roles:  []string{"doctor"},
		}, nil
	})

	req := makeReq(t, map[string]string{"Authorization": fmt.Sprintf("Bearer %s", tok)})
	p, err := a.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate with valid JWT: %v", err)
	}
	if p.UserID != "dr-jones" {
		t.Errorf("UserID: got %q, want dr-jones", p.UserID)
	}
	if len(p.Roles) == 0 || p.Roles[0] != "doctor" {
		t.Errorf("Roles: got %v, want [doctor]", p.Roles)
	}
}

func TestBearerToken_WithJWT_ExpiredToken(t *testing.T) {
	t.Parallel()
	const secret = "realtime-test-secret"
	_ = makeTestJWT(secret, map[string]any{
		"sub": "u1",
		"exp": 1, // always expired
		"iat": 1,
	})

	a := realtime.BearerToken(func(token string) (*realtime.Principal, error) {
		return nil, &realtime.ErrUnauthorized{Reason: "token expired"}
	})

	req := makeReq(t, map[string]string{"Authorization": "Bearer expired.jwt.token"})
	_, err := a.Authenticate(req)
	if err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

// ── Composite ─────────────────────────────────────────────────────────────────

func TestComposite_FirstMatchWins(t *testing.T) {
	t.Parallel()
	a1 := staticAuthenticator{p: &realtime.Principal{UserID: "u1"}}
	a2 := staticAuthenticator{p: &realtime.Principal{UserID: "u2"}}

	comp := realtime.Composite(a1, a2)
	req := makeReq(t, nil)

	p, err := comp.Authenticate(req)
	if err != nil {
		t.Fatalf("Composite: %v", err)
	}
	if p.UserID != "u1" {
		t.Errorf("expected u1 (first match), got %q", p.UserID)
	}
}

func TestComposite_SkipsFailingAuthenticators(t *testing.T) {
	t.Parallel()
	a1 := staticAuthenticator{err: errors.New("first fails")}
	a2 := staticAuthenticator{p: &realtime.Principal{UserID: "u2"}}

	comp := realtime.Composite(a1, a2)
	req := makeReq(t, nil)

	p, err := comp.Authenticate(req)
	if err != nil {
		t.Fatalf("Composite: %v", err)
	}
	if p.UserID != "u2" {
		t.Errorf("expected u2 (second authenticator), got %q", p.UserID)
	}
}

func TestComposite_AllFail_ReturnsError(t *testing.T) {
	t.Parallel()
	a1 := staticAuthenticator{err: errors.New("a1 failed")}
	a2 := staticAuthenticator{err: errors.New("a2 failed")}

	comp := realtime.Composite(a1, a2)
	req := makeReq(t, nil)

	_, err := comp.Authenticate(req)
	if err == nil {
		t.Fatal("expected error when all authenticators fail, got nil")
	}
}

func TestComposite_Empty_DoesNotPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Composite() panicked: %v", r)
		}
	}()
	comp := realtime.Composite()
	req := makeReq(t, nil)
	_, _ = comp.Authenticate(req) // must not panic
}

// ── ErrUnauthorized ───────────────────────────────────────────────────────────

func TestErrUnauthorized_ImplementsError(t *testing.T) {
	t.Parallel()
	err := &realtime.ErrUnauthorized{Reason: "expired"}
	if err.Error() == "" {
		t.Error("ErrUnauthorized.Error() must not return empty string")
	}
}

func TestErrUnauthorized_AsTarget(t *testing.T) {
	t.Parallel()
	original := &realtime.ErrUnauthorized{Reason: "bad token"}
	wrapped := fmt.Errorf("auth: %w", original)

	var target *realtime.ErrUnauthorized
	if !errors.As(wrapped, &target) {
		t.Error("errors.As must find *ErrUnauthorized through wrapping")
	}
	if target.Reason != "bad token" {
		t.Errorf("Reason: got %q, want %q", target.Reason, "bad token")
	}
}

// ── Principal ─────────────────────────────────────────────────────────────────

func TestPrincipal_ZeroValueIsAnonymous(t *testing.T) {
	t.Parallel()
	var p realtime.Principal
	if p.UserID != "" || p.TenantID != "" {
		t.Error("zero Principal should be anonymous (empty UserID, TenantID)")
	}
	if len(p.Roles) != 0 || len(p.Scopes) != 0 {
		t.Error("zero Principal should have no Roles or Scopes")
	}
}

// ── Hub integration: principal flows into VisibilityFunc ─────────────────────

func TestHub_AuthenticatedPrincipal_PassedToVisibilityHook(t *testing.T) {
	// Verifies that the Principal returned by the Authenticator is the
	// same one received by the VisibilityFunc.
	t.Parallel()
	bus := inproc.New()

	var capturedUserID string
	hub := mustHub(t, realtime.HubConfig{
		Bus: bus,
		Authenticator: realtime.BearerToken(func(tok string) (*realtime.Principal, error) {
			return &realtime.Principal{UserID: tok, TenantID: "tenant-1"}, nil
		}),
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			capturedUserID = p.UserID
			return true, nil
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws", bearerHeader("alice"))
	c.subscribe("*")

	publish(t, bus, "test.event", "x/1")
	c.recvTimeout(2 * time.Second)

	// Brief pause to ensure the visibility hook has run.
	time.Sleep(50 * time.Millisecond)

	if capturedUserID != "alice" {
		t.Errorf("VisibilityFunc got UserID=%q, want alice", capturedUserID)
	}
}

func TestHub_AnonymousOnly_VisibilityReceivesEmptyPrincipal(t *testing.T) {
	t.Parallel()
	bus := inproc.New()

	var capturedPrincipal *realtime.Principal
	hub := mustHub(t, realtime.HubConfig{
		Bus:           bus,
		Authenticator: realtime.AnonymousOnly{},
		Visibility: func(p *realtime.Principal, e events.Event) (bool, *events.Event) {
			capturedPrincipal = p
			return true, nil
		},
	})
	ts := newHubTestServer(t, hub)

	c := dialWS(t, ts, "/ws")
	c.subscribe("*")

	publish(t, bus, "public.event", "x/1")
	c.recvTimeout(2 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if capturedPrincipal == nil {
		t.Fatal("VisibilityFunc received nil principal; should receive anonymous principal")
	}
	if capturedPrincipal.UserID != "" {
		t.Errorf("anonymous principal UserID should be empty, got %q", capturedPrincipal.UserID)
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

// makeReq creates a GET /ws request with optional header overrides.
func makeReq(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/ws", nil)
	if err != nil {
		t.Fatalf("makeReq: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// makeReqWithQuery creates a GET /ws?<rawQuery> request.
func makeReqWithQuery(t *testing.T, rawQuery string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/ws?"+rawQuery, nil)
	if err != nil {
		t.Fatalf("makeReqWithQuery: %v", err)
	}
	return req
}

// staticAuthenticator is a test-only Authenticator that returns a fixed result.
type staticAuthenticator struct {
	p   *realtime.Principal
	err error
}

func (s staticAuthenticator) Authenticate(_ *http.Request) (*realtime.Principal, error) {
	return s.p, s.err
}
