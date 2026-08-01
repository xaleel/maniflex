package maniflex

// basemodel_tags.go — per-model configuration of the mfx options on the columns
// BaseModel contributes.
//
// The value is unioned onto the defaults, never replacing them. A full-replace
// form would let mfx:"readonly" fall off created_at by omission — the author
// writes BaseModelTags{"created_at": "filterable"} meaning "also filterable",
// and gets a framework-managed timestamp the client can write. That is the
// "protective directive silently absent" shape tags_unknown.go exists to end,
// and it is not worth reintroducing through a different door.

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// baseModelTagOptions is the per-column allowlist of mfx options BaseModelTags
// accepts.
//
// An allowlist rather than a denylist, so an option added to parseFieldTags
// later is refused on a BaseModel column until someone deliberately admits it.
// A denylist would accept it silently, which is the wrong direction to fail for
// a struct nobody can edit.
//
// index is absent from id: id is the primary key and already indexed, so the
// option would only add a redundant idx_<table>_id with real write cost —
// db/sqlcore/adapter.go:652 and :827 already special-case id for this reason.
// hidden is absent for a related reason: the admin panel links rows by id
// (admin/view.go:188) and the cursor tiebreaker orders by it
// (db/sqlcore/cursor.go:26), so hiding it produces a broken API rather than a
// leaner one.
var baseModelTagOptions = map[string][]string{
	"id":         {"filterable", "sortable"},
	"created_at": {"filterable", "sortable", "index", "hidden"},
	"updated_at": {"filterable", "sortable", "index", "hidden"},
}

// baseModelTagColumns returns the valid BaseModelTags keys in a stable order,
// for error messages.
func baseModelTagColumns() []string {
	return slices.Sorted(maps.Keys(baseModelTagOptions))
}

// applyBaseModelTags unions ModelConfig.BaseModelTags onto the parsed tags of
// the BaseModel columns.
//
// ScanModel calls this before every consumer of the flags it sets:
// rejectHiddenRequired / rejectReadonlyRequired (Hidden, Readonly),
// collectCursorField (Sortable) and buildIndices (Index) all run after it.
//
// Mutating through FieldByDBName is safe: modelIndex caches slice positions
// rather than pointers or values, and this function does not append to Fields.
func (m *ModelMeta) applyBaseModelTags() error {
	if len(m.Config.BaseModelTags) == 0 {
		return nil
	}

	// Map iteration is randomised. Without sorting, a config with two bad
	// entries reports a different error on each run — untestable, and miserable
	// to fix, since correcting the reported one surfaces the other.
	for _, col := range slices.Sorted(maps.Keys(m.Config.BaseModelTags)) {
		allowed, ok := baseModelTagOptions[col]
		if !ok {
			return fmt.Errorf(
				"maniflex: model %q BaseModelTags key %q is not a BaseModel column%s — "+
					"valid keys are %s",
				m.Name, col,
				didYouMean(nearest(strings.ToLower(col), baseModelTagColumns())),
				quotedList(baseModelTagColumns()))
		}
		f := m.FieldByDBName(col)
		if f == nil {
			return fmt.Errorf(
				"maniflex: model %q BaseModelTags configures column %q, which the model "+
					"does not have — that column comes from the embedded maniflex.BaseModel",
				m.Name, col)
		}
		for _, opt := range strings.Split(m.Config.BaseModelTags[col], ",") {
			opt = strings.TrimSpace(opt)
			// An empty part is not a typo: "" and a trailing comma both split to
			// one empty string, exactly as in parseFieldTags.
			if opt == "" {
				continue
			}
			if !slices.Contains(allowed, opt) {
				return fmt.Errorf(
					"maniflex: model %q BaseModelTags[%q] option %q does not apply to the "+
						"%s column%s — allowed: %s",
					m.Name, col, opt, col,
					didYouMean(nearest(strings.ToLower(opt), allowed)),
					strings.Join(allowed, ", "))
			}
			switch opt {
			case "filterable":
				f.Tags.Filterable = true
			case "sortable":
				f.Tags.Sortable = true
			case "index":
				f.Tags.Index = true
			case "hidden":
				f.Tags.Hidden = true
			}
		}
	}
	return nil
}

// didYouMean renders a suggestion clause, or nothing when there is no confident
// guess. nearest returns "" on a tie, so a confident wrong guess is never
// offered — it would send the reader to change something that was not the
// problem.
func didYouMean(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %q?)", s)
}

// howToAllow renders the remedy for a field that is not filterable or sortable.
//
// For a BaseModel column, "add the struct tag" is advice the reader cannot
// follow — the struct lives in the framework, not their model — so the message
// names ModelConfig.BaseModelTags instead.
func howToAllow(dbName, opt string) string {
	if _, ok := baseModelTagOptions[dbName]; ok {
		return fmt.Sprintf("add %q to ModelConfig.BaseModelTags[%q]", opt, dbName)
	}
	return fmt.Sprintf("add mfx:%q to the struct tag", opt)
}

// quotedList renders names as a quoted, comma-separated list.
func quotedList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(out, ", ")
}
