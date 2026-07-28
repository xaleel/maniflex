package maniflex

import "strings"

// ValidateProduction audits the fully configured server for
// production-dangerous defaults. Call it after registering models,
// middleware, actions, and optional routes, and before Start or Handler.
//
// It is intentionally opt-in so development remains convenient. A successful
// result means strict startup validation is enabled, database work and client
// query shapes are bounded, automatic migration is disabled when models exist,
// and every mounted data route has an explicit protected or public access
// decision.
func (c *Server) ValidateProduction() error {
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
		seen := make(map[Operation]bool)
		for _, op := range ops {
			if seen[op] {
				continue
			}
			seen[op] = true
			if !c.hasAccessDecision(meta.Name, op) {
				uncovered = append(uncovered, string(op))
			}
		}
		if len(uncovered) > 0 {
			issues.add("access",
				"model %q has mounted operations without an access decision: %s; "+
					"register matching Pipeline.Auth middleware or call Server.AllowPublic",
				meta.Name, strings.Join(uncovered, ", "))
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
		if c.hasAccessDecision(model, OpAction) || action.AccessControlled || action.AllowPublic {
			continue
		}
		issues.add("access",
			"action %s %s has no access decision; register matching Pipeline.Auth middleware, "+
				"set ActionConfig.AccessControlled, or set ActionConfig.AllowPublic",
			action.Method, action.Path)
	}

	if c.globalSearch != nil &&
		!c.globalSearch.AllowPublic &&
		!c.hasAccessDecision(searchModelName, OpSearch) {
		issues.add("access",
			"global search has no access decision; register Pipeline.Auth for OpSearch or set GlobalSearchConfig.AllowPublic")
	}
}

func (c *Server) hasAccessDecision(model string, op Operation) bool {
	for i := range c.Pipeline.Auth.middlewares {
		if c.Pipeline.Auth.middlewares[i].appliesTo(model, op) {
			return true
		}
	}
	for i := range c.publicAccess {
		marker := registeredMiddleware{cfg: c.publicAccess[i]}
		if marker.appliesTo(model, op) {
			return true
		}
	}
	return false
}
