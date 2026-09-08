package maniflex

// Audit OAS-2 — buildModelPaths wrote each model operation's responses as a
// literal derived from the model's shape, so every status produced by the
// pipeline was absent: a server with auth on Pipeline.Auth documented 401 on a
// custom action (buildActionResponses supplies one by default) and denied it on
// the model route behind the same guard.
//
// The statuses here are derived from configuration rather than assumed, so the
// document describes this deployment: no 401 without an auth middleware, no 412
// without OptimisticLock. Auth is not special-cased — a middleware on the Auth
// step implies 401/403 because refusing is what that step is for, which is the
// same rule that carries an application's own DocumentsResponse declarations.

import "strconv"

// pipelineResponses returns the statuses a model operation can answer with
// beyond the ones its own shape implies, keyed by status code as OpenAPI wants
// them. p may be nil — GenerateSpec is exported and a caller assembling a spec
// by hand has no pipeline to offer.
func pipelineResponses(p *Pipeline, cfg *Config, m *ModelMeta, op Operation) map[string]OASResponse {
	out := make(map[string]OASResponse, 8)

	// Anything can fail. The generated 500 carries no detail because Abort
	// replaces the message of any status >= 500 with its status text.
	out["500"] = errResponse("Internal server error")

	// 412 is reachable only where the DB step compares an If-Match, which is
	// exactly where OptimisticLock is on and the operation writes.
	if m.Config.OptimisticLock && (op == OpUpdate || op == OpDelete) {
		out["412"] = errResponse("If-Match did not match the current ETag")
	}

	// A unique constraint can collide on any write; a FK can be violated by a
	// write that names a missing parent. Delete is the other direction — a child
	// row under a restrict relation refuses the parent's removal.
	if writesRows(op) && (hasUniqueField(m) || len(m.Relations) > 0) {
		out["409"] = errResponse("Unique or foreign-key constraint violated")
	}
	if op == OpDelete && hasRestrictRelation(m) {
		out["409"] = errResponse("Referenced by rows a restrict relation protects")
	}

	// Both limits are refusals the framework issues before the handler, and both
	// are off by default — so document them only where they are configured.
	if cfg.MaxConcurrentRequests > 0 {
		out["503"] = retryAfterResponse("Server busy; the concurrency limit is saturated")
	}
	if cfg.QueryTimeout > 0 {
		out["504"] = errResponse("The database query exceeded Config.QueryTimeout")
	}

	if p == nil {
		return out
	}

	// One rule, two jobs. Every step is scanned for declared responses, and the
	// Auth step additionally implies the pair it exists to produce — so
	// auth.JWTAuth needs no OpenAPI awareness of its own.
	//
	// Declarations are applied last and overwrite: an application that says what
	// its 409 means knows better than the constraint-shaped guess above.
	for _, sr := range p.steps() {
		if sr == nil {
			continue
		}
		for _, mw := range sr.middlewares {
			if !mw.appliesTo(m.Name, op) {
				continue
			}
			if sr == p.Auth {
				if _, declared := out["401"]; !declared {
					out["401"] = errResponse("Authentication required")
				}
				if _, declared := out["403"]; !declared {
					out["403"] = errResponse("Not permitted")
				}
			}
			for status, resp := range mw.cfg.DocumentedResponses {
				out[strconv.Itoa(status)] = resp
			}
		}
	}

	return out
}

// mergeResponses adds derived responses to an operation without displacing the
// ones its shape already declares: a 404 written by buildModelPaths says which
// record was not found, and a generic one would be a downgrade.
func mergeResponses(op *OASOperation, derived map[string]OASResponse) {
	if op == nil {
		return
	}
	if op.Responses == nil {
		op.Responses = make(map[string]OASResponse, len(derived))
	}
	for status, resp := range derived {
		if _, exists := op.Responses[status]; !exists {
			op.Responses[status] = resp
		}
	}
}

// retryAfterResponse is an error response that documents the Retry-After header
// accompanying it — the header being the only part of a 503 a client can act on.
func retryAfterResponse(desc string) OASResponse {
	r := errResponse(desc)
	r.Headers = map[string]OASHeader{
		"Retry-After": {
			Description: "Seconds to wait before retrying.",
			Schema:      &OASSchema{Type: "integer"},
		},
	}
	return r
}

func writesRows(op Operation) bool {
	return op == OpCreate || op == OpUpdate
}

func hasUniqueField(m *ModelMeta) bool {
	for _, f := range m.Fields {
		if f.Tags.Unique {
			return true
		}
	}
	return false
}

func hasRestrictRelation(m *ModelMeta) bool {
	for _, r := range m.Relations {
		if r.OnDelete == OnDeleteRestrict {
			return true
		}
	}
	return false
}

// etagHeader documents the ETag a read carries when OptimisticLock is on: it is
// the value a client must send back as If-Match, so a spec that omits it
// describes half a handshake.
func etagHeader() map[string]OASHeader {
	return map[string]OASHeader{
		"ETag": {
			Description: "Current version of the record. Send it back as If-Match on a " +
				"later PATCH or DELETE to make the write conditional.",
			Schema: &OASSchema{Type: "string"},
		},
	}
}

// ifMatchParameter is the request half of the same handshake. Optional by
// design: a request without If-Match bypasses the check rather than failing, so
// declaring it Required would misstate the contract.
func ifMatchParameter() OASParameter {
	return OASParameter{
		Name: "If-Match",
		In:   "header",
		Description: "ETag of the record the caller expects to write. When present and " +
			"stale the request is refused with 412; when absent the write is unconditional. " +
			`"*" matches any existing record.`,
		Schema: &OASSchema{Type: "string"},
	}
}
