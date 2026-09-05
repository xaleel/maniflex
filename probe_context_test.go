package maniflex

// Audit HTTP-4 — the probe endpoints that run dependency checks share a flight,
// so the context those checks run on must not belong to whichever request opened
// it. That was fixed for /ready (audit M3) and not for /health, which kept the
// request context and applied HealthTimeout unconditionally. Two consequences,
// both silent: a probe that hung up failed every request coalesced behind it,
// and a negative HealthTimeout produced an already-expired context, so /health
// reported a database it never reached as degraded — while /ready, on the same
// configuration, answered ok.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbeContext_SurvivesTheRequestGoingAway(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/health", nil).WithContext(reqCtx)

	ctx, done := probeContext(r, &Config{HealthTimeout: 30 * time.Second})
	defer done()

	cancel() // the orchestrator gives up and drops the connection

	select {
	case <-ctx.Done():
		t.Fatalf("the checks' context died with the request that opened the flight (%v); "+
			"every coalesced waiter is handed that failure", context.Cause(ctx))
	default:
	}
}

// Request values still resolve — only cancellation and the deadline are severed,
// so anything request-scoped a check reads keeps working.
func TestProbeContext_KeepsRequestValues(t *testing.T) {
	type key struct{}
	r := httptest.NewRequest("GET", "/health", nil).
		WithContext(context.WithValue(context.Background(), key{}, "carried"))

	ctx, done := probeContext(r, &Config{HealthTimeout: time.Second})
	defer done()

	if got := ctx.Value(key{}); got != "carried" {
		t.Errorf("request value = %v, want %q", got, "carried")
	}
}

func TestProbeContext_AppliesTheBudget(t *testing.T) {
	r := httptest.NewRequest("GET", "/health", nil)

	ctx, done := probeContext(r, &Config{HealthTimeout: 50 * time.Millisecond})
	defer done()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline; HealthTimeout does not bound the run")
	}
	if d := time.Until(deadline); d <= 0 || d > time.Second {
		t.Errorf("deadline is %v away, want roughly the configured 50ms", d)
	}
}

// Zero cannot reach a handler — ApplyDefaults maps it to 3s — and a negative
// value is refused at startup. The guard is a floor for both, and must not
// produce an already-expired context the way WithTimeout does.
func TestProbeContext_NonPositiveBudgetIsNotAnExpiredContext(t *testing.T) {
	for _, budget := range []time.Duration{0, -1} {
		r := httptest.NewRequest("GET", "/health", nil)
		ctx, done := probeContext(r, &Config{HealthTimeout: budget})
		select {
		case <-ctx.Done():
			t.Errorf("HealthTimeout %v produced an already-cancelled context, so every "+
				"check fails and the endpoint reports a dependency it never reached as down",
				budget)
		default:
		}
		done()
	}
}

func TestCollectRouterIssues_NegativeHealthTimeoutIsRefused(t *testing.T) {
	var issues issueList
	collectRouterIssues(&Config{HealthTimeout: -1}, &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("a negative HealthTimeout was accepted; it means \"no bound\" on /ready and " +
			"\"already expired\" on /health")
	}
	if !strings.Contains(err.Error(), "HealthTimeout") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// Not Strict-gated: the field offers no disable, so a negative value is a
// mistake under any configuration.
func TestCollectRouterIssues_NegativeHealthTimeoutIsNotStrictOnly(t *testing.T) {
	var issues issueList
	collectRouterIssues(&Config{HealthTimeout: -1, Strict: false}, &issues)

	if len(issues) != 1 {
		t.Fatalf("got %d issues, want 1 without Strict", len(issues))
	}
	if issues[0].Strict {
		t.Error("the issue is marked Strict-only, so a default-configuration server would boot")
	}
}

func TestCollectRouterIssues_PositiveHealthTimeoutIsAccepted(t *testing.T) {
	var issues issueList
	collectRouterIssues(&Config{HealthTimeout: 3 * time.Second}, &issues)

	if err := issues.err(); err != nil {
		t.Fatalf("an ordinary HealthTimeout was refused: %v", err)
	}
}
