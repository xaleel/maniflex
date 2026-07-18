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

// ── collectFieldRequirementIssues (10.2) ─────────────────────────────────────

// fieldReqRegistry has one model with a "quota" field and one without.
func fieldReqRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.AddForTest(&ModelMeta{
		Name:   "User",
		Fields: []FieldMeta{{Name: "Quota", Tags: FieldTags{JSONName: "quota", DBName: "quota"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddForTest(&ModelMeta{
		Name:   "Post",
		Fields: []FieldMeta{{Name: "Title", Tags: FieldTags{JSONName: "title", DBName: "title"}}},
	}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func fieldReqPipeline(opts ...MiddlewareOption) *Pipeline {
	p := newPipeline(&defaultSteps{}, &oasDefaultSteps{})
	p.Validate.Register(func(ctx *ServerContext, next func() error) error { return next() }, opts...)
	return p
}

// The case 10.2 exists for: a gate aimed at a specific model, naming a field
// that model does not have. Unambiguously a typo, and the real field is ungated.
func TestFieldRequirement_ScopedModelMissingFieldIsAnError(t *testing.T) {
	p := fieldReqPipeline(ForModel("User"), RequiresField("quotaa"), WithName("quota-gate"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("a gate scoped to User declaring a field User lacks must be an error")
	}
	msg := err.Error()
	for _, want := range []string{"quota-gate", "quotaa", "User", "spelling"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %q", want, msg)
		}
	}
}

// Every model named in ForModel must carry the field — the gate was aimed at
// them specifically.
func TestFieldRequirement_ReportsEachScopedModelSeparately(t *testing.T) {
	p := fieldReqPipeline(ForModel("User", "Post"), RequiresField("quota"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)

	if len(issues) != 1 {
		t.Fatalf("want 1 issue (Post lacks quota, User has it), got %d", len(issues))
	}
	if !strings.Contains(issues[0].Detail, "Post") {
		t.Errorf("the issue should name Post, not User: %q", issues[0].Detail)
	}
}

// A satisfied declaration is silent.
func TestFieldRequirement_SatisfiedDeclarationIsSilent(t *testing.T) {
	p := fieldReqPipeline(ForModel("User"), RequiresField("quota"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)
	if err := issues.err(); err != nil {
		t.Errorf("a declaration every named model satisfies must be silent, got: %v", err)
	}
}

// Unscoped, one model carries the field: legitimate, and exactly the case the
// runtime warning could never distinguish from a typo.
func TestFieldRequirement_UnscopedIsFineIfAnyModelHasTheField(t *testing.T) {
	p := fieldReqPipeline(RequiresField("quota"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)
	if err := issues.err(); err != nil {
		t.Errorf("an unscoped gate that can fire on User must be silent, got: %v", err)
	}
}

// Unscoped and no model has it: the gate cannot fire anywhere, so it is a typo
// however it was registered.
func TestFieldRequirement_UnscopedWithNoMatchingModelIsAnError(t *testing.T) {
	p := fieldReqPipeline(RequiresField("nonexistent"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("a gate no registered model can trigger must be an error")
	}
	if !strings.Contains(err.Error(), "never fire") {
		t.Errorf("error should say the gate can never fire: %q", err.Error())
	}
}

// ForModel naming a model that is not registered is a different problem, and
// reporting "the model lacks the field" would only mislead.
func TestFieldRequirement_UnregisteredModelIsNotThisChecksProblem(t *testing.T) {
	p := fieldReqPipeline(ForModel("Ghost"), RequiresField("quota"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)
	if err := issues.err(); err != nil {
		t.Errorf("an unregistered model must not be reported as missing a field, got: %v", err)
	}
}

// A middleware that declares nothing is not checked — the option is opt-in.
func TestFieldRequirement_NoDeclarationIsNotChecked(t *testing.T) {
	p := fieldReqPipeline(ForModel("Post"))

	var issues issueList
	p.collectFieldRequirementIssues(fieldReqRegistry(t), &issues)
	if err := issues.err(); err != nil {
		t.Errorf("a middleware declaring nothing must not be checked, got: %v", err)
	}
}
