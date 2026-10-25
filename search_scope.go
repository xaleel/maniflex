package maniflex

// search_scope.go — the startup check that answers "is GET /search row-scoped?"
//
// GET /search runs the trimmed pipeline: Auth → scope → handler → Response. Every
// other step is skipped, and scopeMiddlewares drops a ProvidesScope registration
// whose own step the operation skips, because hoisting changes when a middleware
// runs and never whether it does. So a row scope registered the conventional way
// — on the DB step, which is where db.Tenancy's and auth.Enforce's own examples
// put it — is simply absent from /search, and ctx.Search returns every row of
// every GlobalSearchable model to anyone the Auth step let through.
//
// Nothing reported it. collectIneffectiveMiddleware catches a registration whose
// ForOperation filter can never reach its step, but it "never [fires] for an
// unfiltered middleware", and the conventional registration is unfiltered: it is
// perfectly effective on every CRUD operation and silently missing on this one
// (audit AUTH-20).
//
// ctx.Search now applies the forced filters it is handed, so a scope registered
// where OpSearch runs works. This file is about the registrations that are not,
// which no runtime check can see — the request simply arrives unscoped and
// succeeds.

import (
	"fmt"
	"log/slog"

	"github.com/xaleel/maniflex/internal/accessdecision"
)

// searchScopeGap is one (model, registration) pair whose row scope GET /search
// cannot apply: the model is in the search set, the middleware scopes its rows
// for ordinary reads, and it does not run on a search request.
type searchScopeGap struct {
	model      string // the GlobalSearchable model left unscoped
	middleware string // "db.Tenancy", or the registration's WithName, or a placeholder
	step       string // the step it is registered on, for the message
	reason     string // why it does not reach /search
}

// unscopedSearchModels reports every searchable model whose row scope the search
// endpoint cannot apply. It returns nothing when global search is off.
//
// "Scopes this model's rows" is read from two places, because the framework's own
// middleware and an application's say it differently: a marked constructor
// (accessdecision.MarkRowScope) covers db.Tenancy, db.ForceFilter,
// db.ForceFilterVia and auth.Enforce however they were registered, and
// ProvidesScope() covers anything an application wrote and declared.
//
// "Reaches /search" is one condition, and it is the same one for both kinds: the
// step must be one OpSearch runs, and the registration must apply to the
// synthetic "__search" model. compose includes an ordinary middleware under
// exactly that condition and scopeChain re-adds a hoisted one under exactly that
// condition, so there is nothing to distinguish.
func unscopedSearchModels(reg RegistryAccessor, p *Pipeline, gs *GlobalSearchConfig) []searchScopeGap {
	if gs == nil || reg == nil || p == nil {
		return nil
	}
	var gaps []searchScopeGap
	for _, m := range reg.All() {
		if !m.Config.GlobalSearchable || len(m.SearchFields) == 0 {
			continue // not in the search set, so nothing to scope
		}
		for _, sr := range p.steps() {
			for i := range sr.middlewares {
				mw := &sr.middlewares[i]
				label, marked := accessdecision.RowScopeLabel(mw.fn)
				if !marked && !mw.cfg.ProvidesScope {
					continue
				}
				// Does it scope this model's ordinary reads? OpList stands for
				// them: a scope that does not apply to the plain collection read
				// is not one this endpoint is failing to honour.
				if !mw.appliesTo(m.Name, OpList) {
					continue
				}
				if reason := whySearchMisses(mw, sr, m.Name); reason != "" {
					gaps = append(gaps, searchScopeGap{
						model:      m.Name,
						middleware: scopeLabel(label, marked, mw),
						step:       sr.displayName,
						reason:     reason,
					})
				}
			}
		}
	}
	return gaps
}

// whySearchMisses names the reason mw does not run on a search request, or ""
// when it does. The three reasons are distinct remedies, which is why the message
// carries one rather than just reporting the gap.
func whySearchMisses(mw *registeredMiddleware, sr *StepRegistry, model string) string {
	if opSkipsStep(OpSearch, sr.name) {
		return fmt.Sprintf("GET /search does not run the %s step", sr.displayName)
	}
	if len(mw.cfg.Models) > 0 {
		return fmt.Sprintf("it is scoped ForModel(%s) and GET /search runs on the synthetic %q model",
			quoteJoin(mw.cfg.Models), searchModelName)
	}
	if !mw.appliesTo(searchModelName, OpSearch) {
		return fmt.Sprintf("its ForOperation(%v) does not include OpSearch", mw.cfg.Operations)
	}
	return ""
}

// scopeLabel names a registration as well as it can be named: the constructor
// when the framework marked it, else the caller's WithName, else a description of
// where it sits.
func scopeLabel(label string, marked bool, mw *registeredMiddleware) string {
	if marked {
		return label
	}
	if mw.cfg.Name != "" {
		return mw.cfg.Name
	}
	return "a ProvidesScope middleware"
}

// searchScopeDetail renders one gap as the sentence both the warning and the
// strict-mode startup error carry.
func searchScopeDetail(g searchScopeGap) string {
	return fmt.Sprintf(
		"model %q is in the GET /search set and %s on the %s step scopes its rows, but %s — "+
			"so every caller reaches every row of %s through search. Register the scope on "+
			"Pipeline.Auth, unscoped by model, or drop GlobalSearchable from %s",
		g.model, g.middleware, g.step, g.reason, g.model, g.model)
}

// collectSearchScopeIssues refuses to start a strict server whose global search
// is unscoped.
//
// Only under Strict. Outside it this is a warning — see warnUnscopedGlobalSearch
// — because the combination is not a typo the way an unreachable ForOperation is:
// the app got what it wrote for every CRUD operation, and whether the searchable
// model holds anything worth scoping is a question only its author can answer.
func collectSearchScopeIssues(reg RegistryAccessor, p *Pipeline, gs *GlobalSearchConfig,
	strict bool, issues *issueList) {
	if !strict {
		return
	}
	for _, g := range unscopedSearchModels(reg, p, gs) {
		issues.addStrict("search", "%s", searchScopeDetail(g))
	}
}

// warnUnscopedGlobalSearch logs the same gaps at boot on a non-strict server.
//
// It reaches someone who can act: the leak is silent at runtime — a search
// request arrives, is not scoped, and answers 200 — so boot is the only moment
// anything can say so.
func warnUnscopedGlobalSearch(reg RegistryAccessor, p *Pipeline, gs *GlobalSearchConfig,
	cfg *Config, l *slog.Logger) {
	if cfg != nil && cfg.Strict {
		return // already a startup error; a warning as well is just noise
	}
	for _, g := range unscopedSearchModels(reg, p, gs) {
		l.Warn("global search is not row-scoped for this model; every caller reaches every row of "+
			"it through GET /search",
			slog.String("model", g.model),
			slog.String("scope", g.middleware),
			slog.String("registered_on", g.step),
			slog.String("reason", g.reason),
			slog.String("hint", "register the scope on Pipeline.Auth, unscoped by model, or drop "+
				"GlobalSearchable"))
	}
}
