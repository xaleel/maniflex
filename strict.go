package maniflex

import (
	"fmt"
	"strings"
)

// startupIssue is one configuration problem found by the startup validation
// pass. Issues are collected rather than raised on sight so a misconfigured
// application learns everything wrong with it in one boot instead of one
// restart at a time (roadmap 10.1).
type startupIssue struct {
	// Site names the area the problem is in — "relation", "middleware",
	// "files", "static" — so a reader can tell at a glance which subsystem to
	// look at.
	Site string

	// Detail states the problem and, wherever possible, its fix. The existing
	// warnings this replaced all named the remedy; that must not be lost in
	// translation to an error.
	Detail string

	// Strict marks an issue that is only a problem because Config.Strict is on.
	// It is rendered as such, so nobody hunts for a bug in configuration that is
	// legal by default.
	Strict bool
}

// issueList accumulates startup problems across the validation pass.
type issueList []startupIssue

// add records a problem that is always fatal.
func (l *issueList) add(site, format string, args ...any) {
	*l = append(*l, startupIssue{Site: site, Detail: fmt.Sprintf(format, args...)})
}

// addStrict records a problem that is fatal only under Config.Strict. Callers
// gate the call itself; this only marks the rendering.
func (l *issueList) addStrict(site, format string, args ...any) {
	*l = append(*l, startupIssue{Site: site, Detail: fmt.Sprintf(format, args...), Strict: true})
}

// err renders the collected issues as a single error, or nil when there are
// none.
//
// The format is deliberately verbose: this fires once, at boot, in front of
// someone who has to fix it. One problem reads as a sentence; several are
// numbered so none is overlooked.
func (l issueList) err() error {
	if len(l) == 0 {
		return nil
	}
	if len(l) == 1 {
		return fmt.Errorf("maniflex: %s", l.line(l[0]))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "maniflex: %d startup problems:", len(l))
	for i, iss := range l {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, l.line(iss))
	}
	return fmt.Errorf("%s", b.String())
}

// line renders one issue: its site, the detail, and a marker when it is only
// fatal because strict mode is on.
func (issueList) line(iss startupIssue) string {
	suffix := ""
	if iss.Strict {
		suffix = " (Config.Strict)"
	}
	return fmt.Sprintf("[%s] %s%s", iss.Site, iss.Detail, suffix)
}

// validateRegistry runs every whole-registry check and reports all the problems
// it finds at once.
//
// It is the single place startup validation happens. Handler panics on a
// non-empty result, preserving its contract — it has no error return — while
// StartWithContext calls this directly, before migrating, so a misconfigured
// application fails without having altered the schema or started a service.
//
// Every check here needs the complete registry, which is why none of them can
// run at Register time: a relation's target may be registered after the model
// that points at it, and a middleware's effectiveness depends on the pipeline
// being fully assembled.
//
// It is idempotent and cheap, so being called from both paths is fine.
func (c *Server) validateRegistry() error {
	var issues issueList

	// Cross-model resolution first: these produce the problems most likely to
	// explain the others.
	if err := resolveManyToMany(c.registry); err != nil {
		issues.add("relation", "%s", err.Error())
	}
	if err := validateLockScopes(c.registry); err != nil {
		issues.add("lock_scope", "%s", err.Error())
	}
	if err := validateOnDeleteActions(c.registry); err != nil {
		issues.add("relation", "%s", err.Error())
	}

	collectRelationIssues(c.registry, c.cfg.Strict, &issues)
	c.Pipeline.collectIneffectiveMiddleware(&issues)
	collectRouterIssues(&c.cfg, &issues)

	return issues.err()
}
