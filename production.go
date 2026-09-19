package maniflex

import (
	"strings"

	"github.com/xaleel/maniflex/internal/accessdecision"
)

// ValidateProduction audits the fully configured server for
// production-dangerous defaults. Call it after registering models,
// middleware, actions, and optional routes, and before Start or Handler.
//
// It is intentionally opt-in so development remains convenient. A successful
// result means strict startup validation is enabled, database work and client
// query shapes are bounded, automatic migration is disabled when models exist,
// and every mounted data route has an explicit protected or public access
// decision.
//
// It also runs every registry check Start runs — proxy allowlist, blind-index
// keys for encrypted unique fields, signable storage for file_acl:signed fields,
// OpenAPI schema coverage, relations, lock scopes and middleware wiring — so one
// call reports the whole startup posture rather than only the part specific to
// production. Config.Strict is required to pass, and several of those checks are
// findings Strict turns fatal, so they surface here rather than at the first
// boot.
func (c *Server) ValidateProduction() error {
	c.mu.Lock()
	c.productionValidated = true
	c.mu.Unlock()

	var issues issueList
	c.collectRegistryIssues(&issues)
	c.collectProductionIssues(&issues)
	return issues.err()
}

func (c *Server) collectProductionIssues(issues *issueList) {
	if !c.cfg.Strict {
		issues.add("production", "Config.Strict must be enabled")
	}
	if c.cfg.QueryTimeout <= 0 {
		issues.add("production", "Config.QueryTimeout must be positive to bound database work")
	}
	if len(c.registry.All()) > 0 && !c.cfg.DisableAutoMigrate {
		issues.add("production",
			"automatic migration is enabled; set Config.DisableAutoMigrate and run migrations separately")
	}
	c.collectConcurrencyIssues(issues)
	collectProductionQueryLimitIssues(c.cfg.QueryLimits, issues)
	for _, meta := range c.registry.All() {
		if meta.Config.Headless {
			continue
		}
		effective := resolveQueryLimits(c.cfg.QueryLimits, meta.Config.QueryLimits)
		// A per-model override cannot loosen the router-level URI ceiling.
		effective.MaxURLBytes = c.cfg.QueryLimits.MaxURLBytes
		if unbounded := unboundedProductionQueryLimits(effective); len(unbounded) > 0 {
			issues.add("production",
				"model %q QueryLimits leaves production query work unbounded: %s",
				meta.Name, strings.Join(unbounded, ", "))
		}
	}
	if c.globalSearch != nil && c.globalSearch.MaxLimit <= 0 {
		issues.add("production",
			"GlobalSearchConfig.MaxLimit must be positive to bound cross-model search")
	}

	if c.cfg.HTTPAccessControlled {
		if len(c.cfg.HTTPMiddlewares) == 0 {
			issues.add("access",
				"Config.HTTPAccessControlled is true but Config.HTTPMiddlewares is empty")
		}
		return
	}

	c.collectModelAccessIssues(issues)
	c.collectAuxiliaryAccessIssues(issues)
}

// collectConcurrencyIssues requires an explicit ceiling on in-flight requests.
//
// Unset, the connection pool becomes the limit by accident, and it is one that
// queues rather than refuses — so a burst degrades every request instead of
// being paid by the ones that are shed.
func (c *Server) collectConcurrencyIssues(issues *issueList) {
	if c.cfg.MaxConcurrentRequests <= 0 {
		issues.add("production",
			"Config.MaxConcurrentRequests must be positive to bound in-flight requests; "+
				"without it a burst queues on the database pool instead of being refused")
	}
}

func collectProductionQueryLimitIssues(limits QueryLimits, issues *issueList) {
	unbounded := unboundedProductionQueryLimits(limits)
	if len(unbounded) > 0 {
		issues.add("production",
			"Config.QueryLimits leaves production query work unbounded: %s",
			strings.Join(unbounded, ", "))
	}
}

func unboundedProductionQueryLimits(limits QueryLimits) []string {
	values := []struct {
		name  string
		value int
	}{
		{"MaxURLBytes", limits.MaxURLBytes},
		{"MaxFilterClauses", limits.MaxFilterClauses},
		{"MaxFilterGroups", limits.MaxFilterGroups},
		{"MaxFiltersPerGroup", limits.MaxFiltersPerGroup},
		{"MaxSortFields", limits.MaxSortFields},
		{"MaxSelectFields", limits.MaxSelectFields},
		{"MaxIncludes", limits.MaxIncludes},
		{"MaxAggregateSelectFields", limits.MaxAggregateSelectFields},
		{"MaxAggregateGroupFields", limits.MaxAggregateGroupFields},
		{"MaxAggregateFilters", limits.MaxAggregateFilters},
		{"MaxAggregateHaving", limits.MaxAggregateHaving},
		{"MaxAggregateSortFields", limits.MaxAggregateSortFields},
		{"DefaultAggregateRows", limits.DefaultAggregateRows},
		{"MaxAggregateRows", limits.MaxAggregateRows},
	}
	var unbounded []string
	for _, item := range values {
		if item.value <= 0 {
			unbounded = append(unbounded, item.name)
		}
	}
	return unbounded
}

func (c *Server) collectModelAccessIssues(issues *issueList) {
	storageConfigured := c.cfg.FilesConfig.Storage != nil
	for _, meta := range c.registry.All() {
		if meta.Config.Headless {
			continue
		}

		var ops []Operation
		if meta.Config.Singleton {
			ops = append(ops, OpRead, OpUpdate)
		} else {
			ops = append(ops, OpList, OpCreate, OpRead, OpUpdate, OpDelete)
			if meta.Config.ExportEnabled {
				ops = append(ops, OpExport)
			}
			if meta.Config.Versioned {
				ops = append(ops, OpReadHistory)
			}
			if storageConfigured {
				for _, field := range meta.FileFields() {
					if field.Tags.PresignedUpload {
						ops = append(ops, OpPresignUpload)
					}
					if !field.IsFileList() {
						ops = append(ops, OpReadAttachment)
					}
				}
			}
		}

		var uncovered []string
		hint := ""
		seen := make(map[Operation]bool)
		for _, op := range ops {
			if seen[op] {
				continue
			}
			seen[op] = true
			if decided, skipped := c.accessDecision(meta.Name, op); !decided {
				uncovered = append(uncovered, string(op))
				if skipped {
					hint = notADecisionHint
				}
			}
		}
		if len(uncovered) > 0 {
			issues.add("access",
				"model %q has mounted operations without an access decision: %s; "+
					"register matching Pipeline.Auth middleware or call Server.AllowPublic%s",
				meta.Name, strings.Join(uncovered, ", "), hint)
		}
	}
}

func (c *Server) collectAuxiliaryAccessIssues(issues *issueList) {
	if c.cfg.FilesConfig.MountEndpoints &&
		len(c.cfg.FilesConfig.BeforeMiddlewares) == 0 &&
		!c.cfg.FilesConfig.AllowPublic {
		issues.add("access",
			"standalone /files endpoints have no BeforeMiddlewares; configure an access policy or set FilesConfig.AllowPublic")
	}

	for _, action := range c.actions {
		model := actionSyntheticModel(action.Method, action.Path).Name
		decided, skipped := c.accessDecision(model, OpAction)
		if decided || action.AccessControlled || action.AllowPublic {
			continue
		}
		hint := ""
		if skipped {
			hint = notADecisionHint
		}
		issues.add("access",
			"action %s %s has no access decision; register matching Pipeline.Auth middleware, "+
				"set ActionConfig.AccessControlled, or set ActionConfig.AllowPublic%s",
			action.Method, action.Path, hint)
	}

	if c.globalSearch != nil && !c.globalSearch.AllowPublic {
		if decided, skipped := c.accessDecision(searchModelName, OpSearch); !decided {
			hint := ""
			if skipped {
				hint = notADecisionHint
			}
			issues.add("access",
				"global search has no access decision; register Pipeline.Auth for OpSearch "+
					"or set GlobalSearchConfig.AllowPublic%s", hint)
		}
	}
}

// notADecisionHint is appended to an access issue when the only Auth middleware
// covering the route is middleware that decides nothing, so the report explains
// why a route with middleware on it still counts as uncovered.
const notADecisionHint = " (auth.CSRF and auth.AllowAnonymous are registered there, " +
	"but neither decides who may call a route — add an authenticator)"

// accessDecision reports whether someone decided who may reach model/op:
// Pipeline.Auth middleware that applies to it, or an AllowPublic declaration.
// skipped reports whether applicable middleware was passed over for deciding
// nothing, which is what the issue text needs to say.
//
// Any Pipeline.Auth middleware used to count, so auth.CSRF or
// auth.AllowAnonymous alone passed the audit with every route open: the first
// only compares a cookie to a header, and the second only leaves a note for an
// authenticator that was never registered (audit AUTH-6). They mark themselves,
// and are skipped here. Every other middleware still counts, including one an
// application writes itself — the audit cannot see inside it, and a passthrough
// standing in for an app's own auth has always been accepted.
func (c *Server) accessDecision(model string, op Operation) (decided, skipped bool) {
	for i := range c.Pipeline.Auth.middlewares {
		m := &c.Pipeline.Auth.middlewares[i]
		if !m.appliesTo(model, op) {
			continue
		}
		if accessdecision.IsNotADecision(m.fn) {
			skipped = true
			continue
		}
		return true, skipped
	}
	for i := range c.publicAccess {
		marker := registeredMiddleware{cfg: c.publicAccess[i]}
		if marker.appliesTo(model, op) {
			return true, skipped
		}
	}
	return false, skipped
}
