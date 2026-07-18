package maniflex

import (
	"strings"
	"testing"
)

// A single problem reads as a sentence, not a numbered list of one.
func TestIssueList_SingleIssue(t *testing.T) {
	var issues issueList
	issues.add("relation", "Thread.OwnerID points nowhere")

	err := issues.err()
	if err == nil {
		t.Fatal("err() returned nil for a non-empty list")
	}
	msg := err.Error()
	if strings.Contains(msg, "1.") || strings.Contains(msg, "problems") {
		t.Errorf("a lone issue should not be numbered or pluralised: %q", msg)
	}
	if !strings.Contains(msg, "[relation]") || !strings.Contains(msg, "OwnerID") {
		t.Errorf("message should carry the site and the detail: %q", msg)
	}
}

// The point of collecting: a misconfigured app learns everything at once
// instead of one restart at a time.
func TestIssueList_AggregatesEveryProblem(t *testing.T) {
	var issues issueList
	issues.add("relation", "problem one")
	issues.add("middleware", "problem two")
	issues.addStrict("static", "problem three")

	msg := issues.err().Error()
	for _, want := range []string{"3 startup problems", "1.", "2.", "3.",
		"problem one", "problem two", "problem three"} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error missing %q:\n%s", want, msg)
		}
	}
}

// A strict-only issue must say so, or the reader goes hunting for a bug in
// configuration that is legal by default.
func TestIssueList_MarksStrictOnlyIssues(t *testing.T) {
	var issues issueList
	issues.add("relation", "always fatal")
	issues.addStrict("files", "only under strict")

	msg := issues.err().Error()
	lines := strings.Split(msg, "\n")
	for _, ln := range lines {
		if strings.Contains(ln, "always fatal") && strings.Contains(ln, "Config.Strict") {
			t.Errorf("an unconditional issue must not be marked strict-only: %q", ln)
		}
		if strings.Contains(ln, "only under strict") && !strings.Contains(ln, "Config.Strict") {
			t.Errorf("a strict-only issue must be marked: %q", ln)
		}
	}
}

func TestIssueList_EmptyIsNil(t *testing.T) {
	var issues issueList
	if err := issues.err(); err != nil {
		t.Errorf("an empty issue list must produce no error, got %v", err)
	}
}

// ── collectRouterIssues ──────────────────────────────────────────────────────

// Both router checks are legal by default: an unauthenticated /files mount may
// be deliberate, and a missing static dir must not take down a working API.
func TestCollectRouterIssues_SilentWithoutStrict(t *testing.T) {
	cfg := Config{
		FilesConfig: FilesConfig{MountEndpoints: true},
		StaticDir:   "./definitely-does-not-exist",
	}
	var issues issueList
	collectRouterIssues(&cfg, &issues)
	if err := issues.err(); err != nil {
		t.Errorf("router checks must be silent without Config.Strict, got: %v", err)
	}
}

func TestCollectRouterIssues_StrictReportsBoth(t *testing.T) {
	cfg := Config{
		Strict:      true,
		FilesConfig: FilesConfig{MountEndpoints: true},
		StaticDir:   "./definitely-does-not-exist",
	}
	var issues issueList
	collectRouterIssues(&cfg, &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("strict mode should report both the open /files mount and the missing static dir")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BeforeMiddlewares") {
		t.Errorf("the /files issue should name the fix: %q", msg)
	}
	if !strings.Contains(msg, "definitely-does-not-exist") {
		t.Errorf("the static issue should name the offending path: %q", msg)
	}
	if len(issues) != 2 {
		t.Errorf("want 2 issues, got %d: %q", len(issues), msg)
	}
}

// The /files check is about the standalone endpoints. A server that never
// mounts them cannot have left them open.
func TestCollectRouterIssues_UnmountedFilesIsNotAnIssue(t *testing.T) {
	cfg := Config{Strict: true, FilesConfig: FilesConfig{MountEndpoints: false}}
	var issues issueList
	collectRouterIssues(&cfg, &issues)
	if err := issues.err(); err != nil {
		t.Errorf("unmounted /files must not be reported, got: %v", err)
	}
}

// A configured, existing static dir is fine; so is no static dir at all.
func TestCollectRouterIssues_ValidStaticIsSilent(t *testing.T) {
	for _, cfg := range []Config{
		{Strict: true, StaticDir: t.TempDir()},
		{Strict: true, StaticDir: ""},
		{Strict: true, StaticDir: "./nope", StaticDisabled: true},
	} {
		var issues issueList
		collectRouterIssues(&cfg, &issues)
		if err := issues.err(); err != nil {
			t.Errorf("StaticDir=%q disabled=%v should be silent, got: %v",
				cfg.StaticDir, cfg.StaticDisabled, err)
		}
	}
}

// ── collectIneffectiveMiddleware ─────────────────────────────────────────────

// A middleware registered on a step none of its operations reach never runs.
// When it is an authorisation check, that is a silent hole — so this is fatal
// regardless of Config.Strict.
func TestCollectIneffectiveMiddleware_ReportsUnreachable(t *testing.T) {
	p := newPipeline(&defaultSteps{}, &oasDefaultSteps{})
	noop := func(ctx *ServerContext, next func() error) error { return next() }

	// OpAction skips the DB step entirely (see stepsSkippedByOp).
	p.DB.Register(noop, ForOperation(OpAction), WithName("audit"))

	var issues issueList
	p.collectIneffectiveMiddleware(&issues)

	err := issues.err()
	if err == nil {
		t.Fatal("a middleware on a step its operation skips must be reported")
	}
	msg := err.Error()
	if !strings.Contains(msg, "audit") {
		t.Errorf("error should name the middleware: %q", msg)
	}
	if !strings.Contains(msg, "never run") {
		t.Errorf("error should say what the consequence is: %q", msg)
	}
}

// A filter that is effective for even one of its operations is fine, and an
// unfiltered middleware applies everywhere.
func TestCollectIneffectiveMiddleware_EffectiveRegistrationsAreSilent(t *testing.T) {
	p := newPipeline(&defaultSteps{}, &oasDefaultSteps{})
	noop := func(ctx *ServerContext, next func() error) error { return next() }

	p.DB.Register(noop, ForOperation(OpCreate, OpAction), WithName("mixed")) // OpCreate reaches DB
	p.DB.Register(noop, WithName("unfiltered"))                              // all operations
	p.Auth.Register(noop, ForOperation(OpAction), WithName("auth-action"))   // Auth is never skipped

	var issues issueList
	p.collectIneffectiveMiddleware(&issues)
	if err := issues.err(); err != nil {
		t.Errorf("effective registrations must be silent, got: %v", err)
	}
}
