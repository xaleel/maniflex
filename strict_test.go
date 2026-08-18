package maniflex

import (
	"bytes"
	"log/slog"
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

// Audit S1. TrustProxyHeaders on its own believes the leftmost X-Forwarded-For
// entry from any peer, and that entry is the one a client controls. Defensible
// behind a proxy that strips inbound headers, so it stays a warning by default
// and Strict makes it fatal — the same treatment the open /files mount gets.
func TestCollectRouterIssues_TrustProxyHeadersWithoutAllowlistIsStrictIssue(t *testing.T) {
	cfg := Config{Strict: true, TrustProxyHeaders: true}
	var issues issueList
	collectRouterIssues(&cfg, &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("an allowlist-free TrustProxyHeaders must be reported under Strict")
	}
	if !strings.Contains(err.Error(), "TrustedProxies") {
		t.Errorf("the issue should name the fix: %q", err.Error())
	}
}

// Declaring the proxies is the fix, so it must silence the check.
func TestCollectRouterIssues_TrustedProxiesSatisfiesTheStrictCheck(t *testing.T) {
	for _, cfg := range []Config{
		{Strict: true, TrustProxyHeaders: true, TrustedProxies: []string{"10.0.0.0/8"}},
		// The list alone enables resolution; and with neither, it is off entirely.
		{Strict: true, TrustedProxies: []string{"10.0.0.0/8"}},
		{Strict: true},
	} {
		var issues issueList
		collectRouterIssues(&cfg, &issues)
		if err := issues.err(); err != nil {
			t.Errorf("cfg %+v should be silent, got: %v", cfg, err)
		}
	}
}

// Not Strict-gated: an entry that does not parse is unambiguously a mistake, and
// dropping it silently would narrow what is trusted — failing open on exactly
// the requests it was written to cover.
func TestCollectRouterIssues_InvalidTrustedProxyIsAlwaysAnError(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "10.0.0.0/99", ""} {
		cfg := Config{TrustedProxies: []string{bad}} // Strict deliberately off
		var issues issueList
		collectRouterIssues(&cfg, &issues)

		err := issues.err()
		if err == nil {
			t.Errorf("TrustedProxies %q must fail startup even without Strict", bad)
			continue
		}
		if !strings.Contains(err.Error(), "TrustedProxies") {
			t.Errorf("the issue should name the field: %q", err.Error())
		}
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

// ── blind-index key separation (audit S3) ─────────────────────────────────────

// blindIndexRegistry builds a registry whose Patient.SSN is encrypted+unique —
// the only shape that writes a blind-index digest and so the only one the key
// separation question applies to.
func blindIndexRegistry(t *testing.T, unique bool) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := reg.AddForTest(&ModelMeta{
		Name: "Patient",
		Fields: []FieldMeta{{
			Name: "SSN",
			Tags: FieldTags{
				JSONName: "ssn", DBName: "ssn",
				Encrypted: true, Unique: unique,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// fixedIndexProvider advertises a dedicated blind-index key, which is what
// EnvKeyProvider does once IndexKeyID is set.
type fixedIndexProvider struct{ KeyProvider }

func (fixedIndexProvider) BlindIndexKeyID() string { return "blind-index" }

// bareProvider implements no BlindIndexKeyProvider at all, so blindIndexKeyID
// falls back to the field's own encryption key.
type bareProvider struct{ KeyProvider }

// Without a dedicated index key the uniqueness digest is HMAC'd under the field
// encryption key. Nothing breaks on write, which is the problem: the bill
// arrives at the first RotateEncryptionKey, which refuses the model outright —
// by which time the table is full of digests nobody can re-derive.
func TestCollectEncryptionIssues_FallbackBlindIndexIsAStrictIssue(t *testing.T) {
	var issues issueList
	collectEncryptionIssues(blindIndexRegistry(t, true), &bareProvider{}, true, &issues)

	err := issues.err()
	if err == nil {
		t.Fatal("an encrypted+unique field with no dedicated blind-index key must be a strict issue")
	}
	msg := err.Error()
	for _, want := range []string{"Patient", "ssn", "RotateEncryptionKey", "IndexKeyID", "Config.Strict"} {
		if !strings.Contains(msg, want) {
			t.Errorf("issue missing %q: %q", want, msg)
		}
	}
}

// Setting IndexKeyID resolves it: the HMAC key is then a different env var
// holding different bytes, so the digest no longer moves when the encryption
// key rotates — and the two algorithms stop sharing key material.
func TestCollectEncryptionIssues_DedicatedIndexKeyIsSilent(t *testing.T) {
	var issues issueList
	collectEncryptionIssues(blindIndexRegistry(t, true), &fixedIndexProvider{}, true, &issues)

	if len(issues) != 0 {
		t.Fatalf("a configured blind-index key must be silent, got: %v", issues.err())
	}
}

// An encrypted field with no UNIQUE constraint writes no digest, so the
// blind-index key is irrelevant to it.
func TestCollectEncryptionIssues_EncryptedButNotUniqueIsSilent(t *testing.T) {
	var issues issueList
	collectEncryptionIssues(blindIndexRegistry(t, false), &bareProvider{}, true, &issues)

	if len(issues) != 0 {
		t.Fatalf("a non-unique encrypted field needs no blind-index key, got: %v", issues.err())
	}
}

// No KeyProvider means encryption is not in use; the check has nothing to say.
func TestCollectEncryptionIssues_NoProviderIsSilent(t *testing.T) {
	var issues issueList
	collectEncryptionIssues(blindIndexRegistry(t, true), nil, true, &issues)

	if len(issues) != 0 {
		t.Fatalf("no KeyProvider configured must be silent, got: %v", issues.err())
	}
}

// Legal by default: existing applications on the fallback keep booting. Strict
// is what promotes it, exactly as the /files and proxy-header checks do.
func TestCollectEncryptionIssues_SilentWithoutStrict(t *testing.T) {
	var issues issueList
	collectEncryptionIssues(blindIndexRegistry(t, true), &bareProvider{}, false, &issues)

	if len(issues) != 0 {
		t.Fatalf("the fallback is legal without Strict, got: %v", issues.err())
	}
}

// The applications most likely to be on the fallback are the ones that never
// turned Strict on, so the boot warning is what actually reaches them.
func TestWarnBlindIndexFallback_WarnsWithoutStrict(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnBlindIndexFallback(blindIndexRegistry(t, true),
		&Config{KeyProvider: &bareProvider{}}, l)

	out := buf.String()
	if out == "" {
		t.Fatal("no warning logged for the blind-index fallback")
	}
	for _, want := range []string{"Patient", "ssn", "RotateEncryptionKey", "IndexKeyID"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q: %s", want, out)
		}
	}
}

// Under Strict the same condition is already a boot failure; logging it as well
// would have the operator fix a warning that was never the reason they crashed.
func TestWarnBlindIndexFallback_SilentUnderStrictWhereItIsAlreadyFatal(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnBlindIndexFallback(blindIndexRegistry(t, true),
		&Config{Strict: true, KeyProvider: &bareProvider{}}, l)

	if out := buf.String(); out != "" {
		t.Errorf("Strict already fails the boot; the warning is redundant: %s", out)
	}
}

// A configured index key is the fix, so it must silence the warning too.
func TestWarnBlindIndexFallback_DedicatedIndexKeyIsSilent(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnBlindIndexFallback(blindIndexRegistry(t, true),
		&Config{KeyProvider: &fixedIndexProvider{}}, l)

	if out := buf.String(); out != "" {
		t.Errorf("a configured blind-index key must be silent: %s", out)
	}
}

// encBlindModel carries the one field shape the check is about.
type encBlindModel struct {
	BaseModel
	SSN string `json:"ssn" mfx:"encrypted,unique"`
}

// The warning is worth nothing unless boot actually emits it. This is the wiring
// test: a real server, assembled the way an application assembles one.
func TestServerBoot_WarnsAboutTheBlindIndexFallback(t *testing.T) {
	var buf bytes.Buffer
	srv := New(Config{
		Logger:      slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		KeyProvider: &bareProvider{},
	})
	srv.MustRegister(encBlindModel{})

	if _, err := srv.handler(); err != nil {
		t.Fatalf("handler(): %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "IndexKeyID") || !strings.Contains(out, "ssn") {
		t.Errorf("boot did not warn about the blind-index fallback: %s", out)
	}
}

// Under Strict the same configuration must refuse to boot rather than warn.
func TestServerBoot_StrictRefusesTheBlindIndexFallback(t *testing.T) {
	srv := New(Config{Strict: true, KeyProvider: &bareProvider{}})
	srv.MustRegister(encBlindModel{})

	_, err := srv.handler()
	if err == nil {
		t.Fatal("Strict must refuse a model whose blind index falls back to the encryption key")
	}
	for _, want := range []string{"encryption", "ssn", "IndexKeyID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("boot error missing %q: %v", want, err)
		}
	}
}
