# Configurable BaseModel Field Tags Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the mfx options on BaseModel's three columns configurable per model via `ModelConfig.BaseModelTags`, and reduce the built-in defaults to `readonly` only.

**Architecture:** BaseModel's struct tags become `mfx:"readonly"` on all three columns. A new `ModelConfig.BaseModelTags map[string]string` is unioned onto those defaults during `ScanModel`, before any consumer reads the resulting flags. Each column has its own allowlist of permitted options; anything else is a registration error.

**Tech Stack:** Go 1.25.12, standard library only (`slices`, `maps`, `strings`, `fmt`). Tests are stdlib `testing`.

**Spec:** `docs/superpowers/specs/2026-07-31-basemodel-configurable-tags-design.md`

## Global Constraints

- **Do not run `git commit`, `git add`, or any other git write command.** The user has explicitly asked that nothing be committed. Each task ends with a verification step instead of a commit step. Leave all changes in the working tree.
- This repo is a multi-module workspace. The root module is `github.com/xaleel/maniflex`. Separate modules that matter here: `tests/` (e2e), `admin/`, `examples/`. Changes in the root module are picked up by the others through `replace` directives, so a root change can break another module's tests.
- Error messages follow the established house style in this codebase: state what is wrong, then why the wrong thing is dangerous or what the fix is. See `tags_unknown.go:191-204` and `model.go:576-588` for the tone to match.
- `nearest(opt, candidates)` returns `""` on a tie or when nothing is within the edit-distance budget. Never render a suggestion clause when it returns `""` — a confident wrong guess is worse than none (`tags_unknown.go:139-162`).
- New exported API needs a doc comment. New unexported helpers that encode a non-obvious decision need a comment saying *why*, matching the density of the surrounding file.
- **`WithDeletedAt` and `WithIsDeleted` are out of scope.** They keep their current tags (`model.go:70-78`: `readonly,filterable` and `readonly,filterable,default:false`). They are opt-in embeds rather than mandatory ones, and their `filterable` default is load-bearing for soft-delete queries. Do not "helpfully" tighten them to match BaseModel.

---

### Task 1: `BaseModelTags` config field and the resolver

Adds the mechanism without changing any default, so the whole repo stays green. Flipping the defaults is Task 3.

**Files:**
- Create: `basemodel_tags.go`
- Create: `basemodel_tags_test.go`
- Modify: `config.go` — add `BaseModelTags` to `ModelConfig`, after the `CursorField` field (`config.go:266`)
- Modify: `model.go:776-779` — call `applyBaseModelTags` in `ScanModel`

**Interfaces:**
- Consumes: `ModelMeta.FieldByDBName` (`model.go:604`), `ModelMeta.Config`, `nearest(opt string, candidates []string) string` (`tags_unknown.go:139`), `tagsFor(t *testing.T, mfx string) FieldTags` (test helper, `tags_unknown_test.go:16`)
- Produces:
  - `var baseModelTagOptions map[string][]string` — the per-column allowlist. Task 2 reads it to decide whether a column is a BaseModel one.
  - `func (m *ModelMeta) applyBaseModelTags() error`
  - `func baseModelTagColumns() []string` — sorted valid keys
  - `func didYouMean(s string) string` — renders `` ` (did you mean "x"?)` `` or `""`
  - `func quotedList(names []string) string`
  - `ModelConfig.BaseModelTags map[string]string`

- [ ] **Step 1: Write the failing tests**

Create `basemodel_tags_test.go`:

```go
package maniflex

import (
	"strings"
	"testing"
)

type baseTagsModel struct {
	BaseModel
	Name string `json:"name" db:"name"`
}

func scanWithBaseTags(t *testing.T, tags map[string]string) (*ModelMeta, error) {
	t.Helper()
	return ScanModel(baseTagsModel{}, ModelConfig{BaseModelTags: tags})
}

func TestBaseModelTags_EachAllowedOptionApplies(t *testing.T) {
	m, err := scanWithBaseTags(t, map[string]string{
		"id":         "filterable,sortable",
		"created_at": "filterable,sortable,index,hidden",
		"updated_at": "filterable,sortable,index,hidden",
	})
	if err != nil {
		t.Fatalf("every allowed option must register: %v", err)
	}
	for _, col := range []string{"id", "created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if f == nil {
			t.Fatalf("column %q missing from the scanned model", col)
		}
		if !f.Tags.Filterable || !f.Tags.Sortable {
			t.Errorf("%s: filterable=%v sortable=%v, want both true",
				col, f.Tags.Filterable, f.Tags.Sortable)
		}
	}
	for _, col := range []string{"created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if !f.Tags.Index || !f.Tags.Hidden {
			t.Errorf("%s: index=%v hidden=%v, want both true",
				col, f.Tags.Index, f.Tags.Hidden)
		}
	}
}

// The union must never cost the default. A replace-style implementation would
// leave created_at client-writable here, which is the whole reason this is a
// union and not an assignment.
func TestBaseModelTags_ReadonlySurvivesTheUnion(t *testing.T) {
	m, err := scanWithBaseTags(t, map[string]string{
		"id":         "filterable",
		"created_at": "filterable",
		"updated_at": "filterable",
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, col := range []string{"id", "created_at", "updated_at"} {
		if f := m.FieldByDBName(col); !f.Tags.Readonly {
			t.Errorf(`%s lost mfx:"readonly" — BaseModelTags must only widen`, col)
		}
	}
}

func TestBaseModelTags_UnknownKeyRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"createdat": "filterable"})
	if err == nil {
		t.Fatal("an unknown BaseModelTags key must be a registration error")
	}
	if !strings.Contains(err.Error(), `did you mean "created_at"?`) {
		t.Errorf("error should suggest the intended column, got: %v", err)
	}
}

func TestBaseModelTags_DisallowedOptionRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"id": "file"})
	if err == nil {
		t.Fatal(`mfx:"file" on the id column must be a registration error`)
	}
	if !strings.Contains(err.Error(), "allowed: filterable, sortable") {
		t.Errorf("error should list what the id column does accept, got: %v", err)
	}
}

// id is the primary key and already indexed; buildIndices would emit a
// redundant idx_<table>_id with real write cost.
func TestBaseModelTags_IndexOnIDRejected(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"id": "index"})
	if err == nil {
		t.Fatal("index on the id column must be rejected — id is the primary key")
	}
}

func TestBaseModelTags_TypoSuggestsAllowedOption(t *testing.T) {
	_, err := scanWithBaseTags(t, map[string]string{"created_at": "filterble"})
	if err == nil {
		t.Fatal("a misspelt option must be a registration error")
	}
	if !strings.Contains(err.Error(), `did you mean "filterable"?`) {
		t.Errorf("error should suggest the intended option, got: %v", err)
	}
}

// Map iteration is randomised; the error must not be. Without sorted keys a
// config with two bad entries reports a different error on each run, so fixing
// the reported one surfaces the other.
func TestBaseModelTags_ErrorIsDeterministic(t *testing.T) {
	tags := map[string]string{"created_at": "bogus", "updated_at": "alsobogus"}
	var first string
	for i := range 50 {
		_, err := scanWithBaseTags(t, tags)
		if err == nil {
			t.Fatal("two invalid options must be a registration error")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("error varies between runs — keys must be sorted before "+
				"iterating:\n  %s\n  %s", first, err.Error())
		}
	}
}

// Same trap as TestUnknownOpts_EmptyPartsAreNotUnknown: "" and a trailing comma
// both split to one empty part, and neither is a typo.
func TestBaseModelTags_EmptyPartsTolerated(t *testing.T) {
	for _, spec := range []string{"", "filterable,", ",filterable", "filterable,,sortable", ","} {
		if _, err := scanWithBaseTags(t, map[string]string{"id": spec}); err != nil {
			t.Errorf("BaseModelTags[\"id\"]=%q must register — an empty comma-part "+
				"is not a typo: %v", spec, err)
		}
	}
}

// baseModelTagOptions is a hand-kept list of option spellings. If one drifts
// from the parser, BaseModelTags starts rejecting an option that exists — the
// same failure TestKnownOptLists_MatchTheParser guards for the suggestion lists.
func TestBaseModelTagOptions_MatchTheParser(t *testing.T) {
	for col, opts := range baseModelTagOptions {
		for _, opt := range opts {
			if got := tagsFor(t, opt); len(got.UnknownOpts) > 0 {
				t.Errorf("baseModelTagOptions[%q] names %q, which parseFieldTags "+
					"does not recognise", col, opt)
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run 'TestBaseModelTags|TestBaseModelTagOptions' -v`

Expected: compile failure — `undefined: baseModelTagOptions`, and `unknown field BaseModelTags in struct literal of type ModelConfig`.

- [ ] **Step 3: Add the `ModelConfig` field**

In `config.go`, immediately after the `CursorField string` field (ends at `config.go:266`), add:

```go
	// BaseModelTags widens the mfx options on the three columns BaseModel
	// contributes. Keys are DB column names ("id", "created_at",
	// "updated_at"); values use the same comma-separated syntax as an mfx
	// struct tag.
	//
	// BaseModel is framework-owned, so its struct tags are the one place a
	// model author cannot reach. Each column defaults to mfx:"readonly" and
	// nothing more — filterable and sortable widen a model's public query
	// surface, and that is a decision each model should make rather than
	// inherit. This is where it makes it:
	//
	//	server.MustRegister(Post{}, maniflex.ModelConfig{
	//	    BaseModelTags: map[string]string{
	//	        "id":         "filterable,sortable",
	//	        "created_at": "filterable,sortable,index",
	//	    },
	//	})
	//
	// Options are unioned onto the defaults, so this can widen a BaseModel
	// column and can never strip a protective default. Each column accepts
	// only the options meaningful for it; anything else is a registration
	// error:
	//
	//	id          filterable, sortable
	//	created_at  filterable, sortable, index, hidden
	//	updated_at  filterable, sortable, index, hidden
	BaseModelTags map[string]string
```

- [ ] **Step 4: Create `basemodel_tags.go`**

```go
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

// quotedList renders names as a quoted, comma-separated list.
func quotedList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(out, ", ")
}
```

- [ ] **Step 5: Wire it into `ScanModel`**

In `model.go`, find the `id` presence check that currently ends at line 779:

```go
	if meta.FieldByDBName("id") == nil {
		return nil, fmt.Errorf(
			"maniflex: model %q has no field with db column \"id\" (embed maniflex.BaseModel)", meta.Name)
	}
```

Insert immediately after its closing brace:

```go
	// Widen the BaseModel columns per ModelConfig.BaseModelTags before anything
	// reads the flags it sets. The position is load-bearing: the reject*
	// validators (Hidden, Readonly), collectCursorField (Sortable) and
	// buildIndices (Index) all run below.
	if err := meta.applyBaseModelTags(); err != nil {
		return nil, err
	}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test . -run 'TestBaseModelTags|TestBaseModelTagOptions' -v`

Expected: PASS, all nine tests.

- [ ] **Step 7: Verify nothing else regressed**

Run: `go build ./... && go test ./...`

Expected: PASS. Nothing has changed for a model that does not set `BaseModelTags`.

Do **not** commit.

---

### Task 2: Point the "not filterable / not sortable" errors at `BaseModelTags`

`?sort=created_at:desc` will start returning 400 in Task 3. The current message tells the caller to add a struct tag — advice they cannot follow, because `BaseModel` is framework-owned. This task fixes the messages first, so Task 3's fallout is self-explanatory.

**Files:**
- Modify: `basemodel_tags.go` — add `howToAllow`
- Modify: `filter.go:330`, `filter.go:351`, `filter.go:386`, `filter.go:433`
- Modify: `query.go:261`
- Modify: `cursor.go:473-477`
- Modify: `basemodel_tags_test.go` — add the two tests below

**Interfaces:**
- Consumes: `baseModelTagOptions` (Task 1)
- Produces: `func howToAllow(dbName, opt string) string` — renders the remedy clause for a field that lacks `opt`

No existing test asserts these strings (verified: `tests/e2e/filter_test.go:348,406`, `tests/e2e/sort_relation_test.go:99` and `tests/e2e/validation_test.go:199` only mention them in comments). `cursor_field_test.go:67` asserts the substring `"sortable"`, which the new wording keeps.

- [ ] **Step 1: Write the failing tests**

Append to `basemodel_tags_test.go`:

```go
// The remedy has to name a knob the reader can actually turn. BaseModel is
// framework-owned, so "add mfx:\"sortable\" to the struct tag" points at a
// struct in the framework's own source tree.
func TestHowToAllow_BaseModelColumnNamesTheConfig(t *testing.T) {
	got := howToAllow("created_at", "sortable")
	if !strings.Contains(got, "ModelConfig.BaseModelTags") {
		t.Errorf("howToAllow(created_at) = %q, want it to name ModelConfig.BaseModelTags", got)
	}
	if !strings.Contains(got, `"created_at"`) {
		t.Errorf("howToAllow(created_at) = %q, want it to name the column", got)
	}
}

func TestHowToAllow_OrdinaryColumnNamesTheStructTag(t *testing.T) {
	got := howToAllow("title", "filterable")
	if !strings.Contains(got, `mfx:"filterable"`) {
		t.Errorf("howToAllow(title) = %q, want it to name the struct tag", got)
	}
	if strings.Contains(got, "BaseModelTags") {
		t.Errorf("howToAllow(title) = %q, must not mention BaseModelTags for an "+
			"ordinary column", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test . -run TestHowToAllow -v`

Expected: compile failure — `undefined: howToAllow`.

- [ ] **Step 3: Add `howToAllow` to `basemodel_tags.go`**

Append to `basemodel_tags.go`:

```go
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
```

- [ ] **Step 4: Rewrite the six call sites**

`filter.go:329-331` — replace:

```go
	if !f.Tags.Filterable {
		return nil, fmt.Errorf("field %q on model %s is not filterable (add mfx:\"filterable\" to the struct tag)", fieldPath, model.Name)
	}
```

with:

```go
	if !f.Tags.Filterable {
		return nil, fmt.Errorf("field %q on model %s is not filterable (%s)",
			fieldPath, model.Name, howToAllow(f.Tags.DBName, "filterable"))
	}
```

`filter.go:350-352` — replace:

```go
		if !f.Tags.Filterable {
			return nil, fmt.Errorf("field %q on model %s is not filterable (add mfx:\"filterable\" to the struct tag)", relKey, model.Name)
		}
```

with:

```go
		if !f.Tags.Filterable {
			return nil, fmt.Errorf("field %q on model %s is not filterable (%s)",
				relKey, model.Name, howToAllow(f.Tags.DBName, "filterable"))
		}
```

`filter.go:385-387` — replace:

```go
	if !nf.Tags.Filterable {
		return nil, fmt.Errorf("field %q on related model %s is not filterable", nestedField, relMeta.Name)
	}
```

with:

```go
	if !nf.Tags.Filterable {
		return nil, fmt.Errorf("field %q on related model %s is not filterable (%s)",
			nestedField, relMeta.Name, howToAllow(nf.Tags.DBName, "filterable"))
	}
```

`filter.go:432-434` — replace:

```go
	if !nf.Tags.Sortable {
		return SortExpr{}, fmt.Errorf("field %q on related model %s is not sortable", nestedField, relMeta.Name)
	}
```

with:

```go
	if !nf.Tags.Sortable {
		return SortExpr{}, fmt.Errorf("field %q on related model %s is not sortable (%s)",
			nestedField, relMeta.Name, howToAllow(nf.Tags.DBName, "sortable"))
	}
```

`query.go:260-262` — replace:

```go
			if !f.Tags.Sortable {
				return nil, fmt.Errorf("field %q is not sortable", name)
			}
```

with:

```go
			if !f.Tags.Sortable {
				return nil, fmt.Errorf("field %q is not sortable (%s)",
					name, howToAllow(f.Tags.DBName, "sortable"))
			}
```

`cursor.go:473-477` — replace:

```go
	if !f.Tags.Sortable {
		return fmt.Errorf(
			"maniflex: model %q cursor_field %q must be mfx:\"sortable\" (keyset pagination orders by it)",
			m.Name, raw)
	}
```

with:

```go
	if !f.Tags.Sortable {
		return fmt.Errorf(
			"maniflex: model %q cursor_field %q must be sortable (keyset pagination "+
				"orders by it) — %s",
			m.Name, raw, howToAllow(f.Tags.DBName, "sortable"))
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test . -run 'TestHowToAllow|TestCollectCursorField' -v`

Expected: PASS.

- [ ] **Step 6: Verify nothing else regressed**

Run: `go build ./... && go test ./...`

Then the e2e module: `cd tests && go test ./e2e/ && cd ..`

Expected: PASS in both. Only message text changed; no test asserts these strings.

Do **not** commit.

---

### Task 3: Flip the BaseModel defaults to readonly-only

This is the breaking change. The root module goes red partway through this task and is green again by the end.

**Files:**
- Modify: `model.go:21-24` — the `BaseModel` struct tags
- Modify: `basemodel_tags_test.go` — add the golden-defaults test
- Modify: `tags_unknown_test.go:255-285` — `cursorEmbedModel` and `TestScanModel_BaseModelEmbedTagsSurvive`

**Interfaces:**
- Consumes: `ModelConfig.BaseModelTags`, `applyBaseModelTags` (Task 1)
- Produces: no new API. After this task, a bare model's BaseModel columns carry `Readonly` only.

- [ ] **Step 1: Write the failing golden-defaults test**

Append to `basemodel_tags_test.go`:

```go
// The defaults are the whole point of the change: filterable and sortable widen
// a model's public query surface, so they are opt-in per model rather than
// inherited by every model that embeds BaseModel. This test is what fails if a
// default is quietly reintroduced later.
func TestBaseModel_DefaultsAreReadonlyOnly(t *testing.T) {
	m, err := ScanModel(baseTagsModel{}, ModelConfig{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, col := range []string{"id", "created_at", "updated_at"} {
		f := m.FieldByDBName(col)
		if f == nil {
			t.Fatalf("column %q missing from the scanned model", col)
		}
		if !f.Tags.Readonly {
			t.Errorf(`%s: readonly=false, want true — BaseModel columns are `+
				`framework-managed`, col)
		}
		if f.Tags.Filterable || f.Tags.Sortable || f.Tags.Index || f.Tags.Hidden {
			t.Errorf("%s: filterable=%v sortable=%v index=%v hidden=%v — all must "+
				"be false by default; opt in via ModelConfig.BaseModelTags",
				col, f.Tags.Filterable, f.Tags.Sortable, f.Tags.Index, f.Tags.Hidden)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test . -run TestBaseModel_DefaultsAreReadonlyOnly -v`

Expected: FAIL — `created_at: filterable=true sortable=true ...` and `updated_at: ... sortable=true ...`, plus `id: readonly=false, want true`.

- [ ] **Step 3: Flip the struct tags**

In `model.go`, replace lines 21-24:

```go
type BaseModel struct {
	ID        string    `json:"id"         db:"id"`
	CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly,filterable,sortable"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly,sortable"`
```

with:

```go
type BaseModel struct {
	ID        string    `json:"id"         db:"id"         mfx:"readonly"`
	CreatedAt time.Time `json:"created_at" db:"created_at" mfx:"readonly"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at" mfx:"readonly"`
```

Also update the doc comment above the struct (`model.go:17-20`) to say what the defaults now are:

```go
// BaseModel provides the standard id / created_at / updated_at columns.
// Embed it in every model struct you register, else registering model fails
// `CreatedAt` and `UpdatedAt` are auto-injected.
// If edited here, make sure the names are edited in the `injectTimestamp` function
//
// All three columns default to mfx:"readonly" and nothing more. filterable and
// sortable widen a model's public query surface, so a model opts into them
// explicitly via ModelConfig.BaseModelTags rather than inheriting them:
//
//	server.MustRegister(Post{}, maniflex.ModelConfig{
//	    BaseModelTags: map[string]string{"created_at": "filterable,sortable"},
//	})
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test . -run TestBaseModel_DefaultsAreReadonlyOnly -v`

Expected: PASS.

- [ ] **Step 5: Fix `cursorEmbedModel`**

`tags_unknown_test.go:255-258` declares a model whose embed carries `mfx:"cursor_field:created_at"`. With `created_at` no longer sortable, `collectCursorField` now rejects it — but the test asserts it registers. The model's *point* is that the embed tag survives the unknown-option check, so keep that and give it the config it now needs.

Replace `tags_unknown_test.go:255-258`:

```go
type cursorEmbedModel struct {
	BaseModel `mfx:"cursor_field:created_at"`
	Name      string `json:"name"`
}
```

with:

```go
// cursor_field names created_at, which is not sortable by default — the model
// has to opt in through BaseModelTags. The pairing is deliberate: a model's
// query surface is exactly what its config says, so cursor_field does not
// implicitly widen it.
type cursorEmbedModel struct {
	BaseModel `mfx:"cursor_field:created_at"`
	Name      string `json:"name"`
}

var cursorEmbedConfig = ModelConfig{
	BaseModelTags: map[string]string{"created_at": "sortable"},
}
```

Then in `TestScanModel_BaseModelEmbedTagsSurvive`, replace the `m3` block (`tags_unknown_test.go:278-284`):

```go
	m3, err := ScanModel(cursorEmbedModel{}, ModelConfig{})
```

with:

```go
	m3, err := ScanModel(cursorEmbedModel{}, cursorEmbedConfig)
```

- [ ] **Step 6: Add a test pinning the cursor pairing**

Append to `basemodel_tags_test.go`:

```go
// cursor_field does not implicitly grant sortable. The missing half is a loud
// registration error rather than a silent widening, so the model's query
// surface stays exactly what its config says.
func TestBaseModelTags_CursorFieldStillNeedsSortable(t *testing.T) {
	_, err := ScanModel(cursorEmbedModel{}, ModelConfig{})
	if err == nil {
		t.Fatal("cursor_field:created_at without BaseModelTags must fail to register")
	}
	if !strings.Contains(err.Error(), "sortable") {
		t.Errorf("error should name the missing option, got: %v", err)
	}
	if !strings.Contains(err.Error(), "BaseModelTags") {
		t.Errorf("error should name the knob that fixes it, got: %v", err)
	}

	if _, err := ScanModel(cursorEmbedModel{}, cursorEmbedConfig); err != nil {
		t.Fatalf("cursor_field:created_at with the matching BaseModelTags must "+
			"register: %v", err)
	}
}
```

- [ ] **Step 7: Run the root module suite and fix the remaining fallout**

Run: `go build ./... && go test ./...`

Expected initially: FAIL. Known sites are handled in Steps 5-6. For anything else that fails, the cause is one of exactly two things:

1. A test model relies on `created_at` / `updated_at` being filterable or sortable → add the matching `ModelConfig.BaseModelTags` at its `ScanModel` / `Register` call.
2. A test asserts a `FieldTags` flag on a BaseModel column → update the expectation to the new default.

Fixtures already known to be unaffected, so do not "fix" them: `filter_time_test.go:21-22` and `cursor_test.go:62` build `FieldMeta` by hand with explicit `Tags`, bypassing `ScanModel` entirely. `basemodel_carrier_test.go:70-87` asserts the column *set* and index paths, not tag flags.

Re-run until green: `go test ./...`

Expected: PASS.

Do **not** commit.

---

### Task 4: Fix the e2e module

The `tests/` module consumes the root module through a `replace` directive, so it went red the moment Task 3 landed.

**Files:**
- Modify: `tests/e2e/cursor_pagination_test.go:26-48`
- Modify: `tests/e2e/fts_test.go:20-40`
- Modify: `tests/e2e/export_test.go:21-29`
- Modify: `tests/e2e/testutil/models.go:164-166`
- Create: `tests/e2e/basemodel_tags_test.go`

**Interfaces:**
- Consumes: `ModelConfig.BaseModelTags` (Task 1), `testutil.NewServer(t, testutil.Options{Models: []any{...}})`.
- `testutil` passes `Models` straight to `server.MustRegister(models...)` (`tests/e2e/testutil/server.go:139`), and `flattenArgs` binds a `ModelConfig` to the model that precedes it at any nesting depth (`server.go:1370-1402`). So a `maniflex.ModelConfig{...}` element placed directly after a model in the `Models` slice configures that model. `export_test.go:24-27` already uses this shape.

- [ ] **Step 1: Run the e2e suite to see the failures**

Run: `cd tests && go test ./e2e/ 2>&1 | head -50`

Expected: FAIL. `SearchDoc` and `CursorByTime` fail at registration; `TestExport_FilterAndSortApplied` and the `created_at` filter case in `filter_test.go` fail at request time.

- [ ] **Step 2: Fix `CursorByTime`**

In `tests/e2e/cursor_pagination_test.go`, replace the `CursorByTime` declaration and `cursorServer` (lines 26-48):

```go
// CursorByTime opts in via a cursor_field tag on the embedded BaseModel, the
// canonical created_at case.
type CursorByTime struct {
	maniflex.BaseModel `mfx:"cursor_field:created_at"`
	Name               string `json:"name" db:"name" mfx:"required"`
}

func cursorServer(t *testing.T) *testutil.Server {
	t.Helper()
	// Register a plain model too so the "not enabled" path can be probed.
	return testutil.NewServer(t, testutil.Options{
		Models: []any{CursorEvent{}, CursorByTime{}, CursorAtTime{}, testutil.User{}},
	})
}
```

with:

```go
// CursorByTime opts in via a cursor_field tag on the embedded BaseModel, the
// canonical created_at case. created_at is not sortable by default, so the
// model also opts in through BaseModelTags — cursor_field does not widen the
// query surface on its own.
type CursorByTime struct {
	maniflex.BaseModel `mfx:"cursor_field:created_at"`
	Name               string `json:"name" db:"name" mfx:"required"`
}

// cursorByTimeConfig grants created_at the sortable that collectCursorField
// requires, plus the index keyset pagination wants anyway.
var cursorByTimeConfig = maniflex.ModelConfig{
	BaseModelTags: map[string]string{"created_at": "sortable,index"},
}

func cursorServer(t *testing.T) *testutil.Server {
	t.Helper()
	// Register a plain model too so the "not enabled" path can be probed.
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			CursorEvent{},
			CursorByTime{}, cursorByTimeConfig,
			CursorAtTime{},
			testutil.User{},
		},
	})
}
```

- [ ] **Step 3: Fix `SearchDoc`**

In `tests/e2e/fts_test.go`, replace `ftsServer` (line 37-40):

```go
func ftsServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{Models: []any{SearchDoc{}, PlainDoc{}}})
}
```

with:

```go
func ftsServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			// created_at is the cursor field, which collectCursorField requires
			// to be sortable — not a default since BaseModel columns are
			// readonly-only.
			SearchDoc{}, maniflex.ModelConfig{
				BaseModelTags: map[string]string{"created_at": "sortable,index"},
			},
			PlainDoc{},
		},
	})
}
```

- [ ] **Step 4: Fix `ExportableRow`**

`TestExport_FilterAndSortApplied` (`export_test.go:106`) sorts by `created_at`. In `tests/e2e/export_test.go`, replace `exportServer` (lines 21-29):

```go
func exportServer(t *testing.T, maxRows int) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.ExportableRow{},
			maniflex.ModelConfig{ExportEnabled: true, MaxExportRows: maxRows},
		},
	})
}
```

with:

```go
func exportServer(t *testing.T, maxRows int) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			testutil.ExportableRow{},
			maniflex.ModelConfig{
				ExportEnabled: true,
				MaxExportRows: maxRows,
				// TestExport_FilterAndSortApplied sorts by created_at.
				BaseModelTags: map[string]string{"created_at": "sortable"},
			},
		},
	})
}
```

- [ ] **Step 5: Fix `User` for the `created_at` filter case**

`tests/e2e/filter_test.go:59` filters `/users` on `created_at`. `User` is registered through `DefaultModels()`. In `tests/e2e/testutil/models.go`, replace lines 164-166:

```go
func DefaultModels() []any {
	return []any{User{}, Post{}, Comment{}, Tag{}}
}
```

with:

```go
func DefaultModels() []any {
	return []any{
		// filter_test.go filters users on created_at, which is not filterable by
		// default — BaseModel columns are readonly-only and opt in per model.
		User{}, maniflex.ModelConfig{
			BaseModelTags: map[string]string{"created_at": "filterable,sortable"},
		},
		Post{}, Comment{}, Tag{},
	}
}
```

- [ ] **Step 6: Add e2e coverage for the new behaviour**

Create `tests/e2e/basemodel_tags_test.go`:

```go
package e2e

import (
	"net/http"
	"testing"

	"github.com/xaleel/maniflex"
	"github.com/xaleel/maniflex/tests/e2e/testutil"
)

// TightDoc takes the defaults: readonly BaseModel columns, no query surface.
type TightDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

// WideDoc opts created_at and id back into the query surface.
type WideDoc struct {
	maniflex.BaseModel
	Name string `json:"name" db:"name" mfx:"required"`
}

func baseTagsServer(t *testing.T) *testutil.Server {
	t.Helper()
	return testutil.NewServer(t, testutil.Options{
		Models: []any{
			TightDoc{},
			WideDoc{}, maniflex.ModelConfig{
				BaseModelTags: map[string]string{
					"id":         "filterable,sortable",
					"created_at": "filterable,sortable",
				},
			},
		},
	})
}

func TestBaseModelTags_SortRejectedByDefault(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	srv.POST("/tight_docs", map[string]any{"name": "a"}).AssertStatus(http.StatusCreated)

	srv.GET("/tight_docs?sort=created_at:desc").AssertStatus(http.StatusBadRequest)
	srv.GET("/tight_docs?filter=created_at:gte:2000-01-01").AssertStatus(http.StatusBadRequest)
}

func TestBaseModelTags_SortAllowedWhenOptedIn(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	srv.POST("/wide_docs", map[string]any{"name": "a"}).AssertStatus(http.StatusCreated)
	srv.POST("/wide_docs", map[string]any{"name": "b"}).AssertStatus(http.StatusCreated)

	srv.GET("/wide_docs?sort=created_at:desc").AssertStatus(http.StatusOK)
	srv.GET("/wide_docs?sort=id:asc").AssertStatus(http.StatusOK)
	srv.GET("/wide_docs?filter=created_at:gte:2000-01-01").AssertStatus(http.StatusOK)
}

// id is readonly now, so a client-supplied id is stripped rather than honoured.
func TestBaseModelTags_ClientSuppliedIDIsIgnored(t *testing.T) {
	t.Parallel()
	srv := baseTagsServer(t)
	resp := srv.POST("/tight_docs", map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "name": "a",
	})
	resp.AssertStatus(http.StatusCreated)
	if got := resp.ID(); got == "11111111-1111-1111-1111-111111111111" {
		t.Error(`id is mfx:"readonly" — a client-supplied id must not be honoured`)
	}
}
```

- [ ] **Step 7: Run the e2e suite**

Run: `cd tests && go test ./e2e/ -run 'TestBaseModelTags|TestCursor|TestFTS|TestExport|TestFilter' -v`

Expected: PASS.

- [ ] **Step 8: Run the whole e2e module and fix any remaining fallout**

Run: `cd tests && go test ./...`

Expected: PASS. Any remaining failure is a model that needs `BaseModelTags` for a `created_at` / `updated_at` / `id` sort or filter it performs — add it at the model's registration site, as in Steps 2-5.

Do **not** commit.

---

### Task 5: Audit the remaining modules and framework-owned models

`admin/`, `examples/`, and the framework's own models are separate compilation units that Task 3 changed the behaviour of. The admin failure is *silent* — no error, just a missing sort dropdown — so it needs a deliberate look rather than a test run.

**Files:**
- Modify: `jobs/maniflex/mount.go` — `StatusModel` registration
- Modify: `pkg/ledger/ledger.go` — `LedgerEntry`, `LedgerLine` registration
- Inspect: `admin/util.go:115` (`sortOptions`), `admin/handler.go:286` (`buildFilters`), `admin/admin_test.go`

**Interfaces:**
- Consumes: `ModelConfig.BaseModelTags` (Task 1)
- Produces: no new API.

- [ ] **Step 1: Build and test every module**

Run each, noting failures:

```bash
go test ./...
cd tests && go test ./... && cd ..
cd admin && go test ./... && cd ..
cd examples && go build ./... && cd ..
```

Expected: `admin` and `examples` may fail. Fix each failure by adding the needed `BaseModelTags` at the model's registration site.

- [ ] **Step 2: Audit `jobs/maniflex/mount.go`**

`StatusModel` (`jobs/maniflex/mount.go:94-106`) is a framework-provided model users list in an admin panel and page through. It has `StartedAt` / `CompletedAt` tagged `readonly,filterable,sortable`, but its `created_at` just lost both. A job-status table is read newest-first; losing that sort is a real regression, not a tightening.

At `jobs/maniflex/mount.go:146-152`, replace:

```go
	if err := server.Register(StatusModel{}, maniflex.ModelConfig{
		TableName: opt.TableName,
		Middleware: &maniflex.ModelMiddleware{
			Validate: []maniflex.MiddlewareFunc{writeBlocker},
			Auth:     []maniflex.MiddlewareFunc{makeForceFilter(opt.AdminRole)},
		},
	}); err != nil {
```

with:

```go
	if err := server.Register(StatusModel{}, maniflex.ModelConfig{
		TableName: opt.TableName,
		// A job-status list is read newest-first, so created_at stays part of
		// the query surface. BaseModel columns are readonly-only by default.
		BaseModelTags: map[string]string{"created_at": "filterable,sortable,index"},
		Middleware: &maniflex.ModelMiddleware{
			Validate: []maniflex.MiddlewareFunc{writeBlocker},
			Auth:     []maniflex.MiddlewareFunc{makeForceFilter(opt.AdminRole)},
		},
	}); err != nil {
```

- [ ] **Step 3: Audit `pkg/ledger/ledger.go`**

`LedgerEntry` has its own `Date` field tagged `filterable,sortable`, so ledger queries have a business-time axis that does not depend on `created_at`. Read `pkg/ledger/ledger.go` and confirm no query path sorts or filters on `created_at` / `updated_at`.

If one does, add `BaseModelTags` at the registration site. If none does, leave it — the tighter default is correct here and nothing needs to change.

- [ ] **Step 4: Check the admin panel's silent loss**

`admin/util.go:115 sortOptions` iterates `Tags.Sortable` and `admin/handler.go:286 buildFilters` iterates `Tags.Filterable`. Neither errors when a column is absent — the dropdown entry simply disappears.

Run: `cd admin && go test ./... && cd ..`

Expected: PASS (the tests assert structure, not specific columns). Then read `admin/admin_test.go` and confirm no test asserts a `created_at` entry in the sort or filter lists. If one does, register that fixture with the matching `BaseModelTags`.

- [ ] **Step 5: Verify every module is green**

```bash
go build ./... && go test ./...
cd tests && go test ./... && cd ..
cd admin && go test ./... && cd ..
cd examples && go build ./... && cd ..
```

Expected: PASS in all four.

Do **not** commit.

---

### Task 6: Documentation, examples, and CHANGELOG

15 places document or demonstrate the old defaults. Every one of them now shows an API call that returns 400.

**Files:**
- Modify: `docs/src/defining-your-api/models.md:51-64` — reproduces the struct verbatim
- Modify: `docs/src/defining-your-api/tags.md`
- Modify: `docs/src/using-the-api/querying.md:81,111,145,228,235,329`
- Modify: `docs/src/index.md:23`
- Modify: `docs/src/getting-started.md:129`
- Modify: `docs/src/example-1.md:155`
- Modify: `docs/src/advanced-topics/export.md:48`
- Modify: `docs/src/reference/ai-agents.md:268`
- Modify: `docs/llms.txt`
- Modify: `README.md:77`
- Modify: `examples/blog.go:53-63,131`
- Modify: `examples/analytics.go:798`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: everything from Tasks 1-5. No code changes here beyond the example programs.

- [ ] **Step 1: Rewrite the BaseModel reference section**

`docs/src/defining-your-api/models.md:51-64` reproduces the struct. Replace the code block and the bullets under it with the new tags, then add a subsection documenting `BaseModelTags` — the per-column allowlist table from the spec, and this example:

```go
server.MustRegister(Post{}, maniflex.ModelConfig{
    BaseModelTags: map[string]string{
        "id":         "filterable,sortable",
        "created_at": "filterable,sortable,index",
    },
})
```

State plainly that options are unioned onto `readonly` and cannot remove it, and give the per-column allowlist:

| Column | Accepts |
| --- | --- |
| `id` | `filterable`, `sortable` |
| `created_at` | `filterable`, `sortable`, `index`, `hidden` |
| `updated_at` | `filterable`, `sortable`, `index`, `hidden` |

- [ ] **Step 2: Fix every doc example that sorts or filters on a BaseModel column**

For each of the sites listed under **Files**, either change the example to use a model field that is filterable/sortable in that example's own model, or add the `BaseModelTags` registration next to it. Prefer adding the registration where the page already shows a `MustRegister` call — it teaches the new knob at the point of confusion.

`docs/src/using-the-api/querying.md` carries six of them and is the page a reader hits when a sort 400s, so it needs the `BaseModelTags` explanation inline, not just a corrected example.

- [ ] **Step 3: Update the runnable examples**

`examples/blog.go:131` and `examples/analytics.go:798` print usage lines advertising `?sort=created_at:desc`. Add `BaseModelTags` to those models' registrations so the printed URL actually works when someone runs the example.

- [ ] **Step 4: Add the CHANGELOG entry**

Add a **Breaking** entry under the current unreleased heading in `CHANGELOG.md`, matching the terse one-line-per-change style of the surrounding entries. It must cover:

- `BaseModel`'s three columns now default to `mfx:"readonly"` only; `filterable` and `sortable` are no longer inherited.
- `ID` is now `readonly`, so a client-supplied `id` in a create body is ignored rather than honoured.
- New `ModelConfig.BaseModelTags` opts columns back in, with the per-column allowlist.
- `cursor_field` on a BaseModel column now requires a matching `BaseModelTags` entry.

- [ ] **Step 5: Verify docs build and examples compile**

```bash
cd examples && go build ./... && cd ..
./docs/mdbook.exe build docs
```

`docs/mdbook.exe` is vendored in the repo (added in commit `93b99ed` to fix docs CI). Expected: the book builds with no broken-link or missing-file errors.

- [ ] **Step 6: Final full verification**

```bash
go build ./... && go test ./...
cd tests && go test ./... && cd ..
cd admin && go test ./... && cd ..
cd examples && go build ./... && cd ..
```

Expected: PASS in all four.

Then confirm nothing was committed:

```bash
git status
```

Expected: modified and untracked files present in the working tree, and no new commits on `main`.

---

## Open item for the user

The spec deliberately leaves the **version number** undecided. This is a breaking change across every module, and the release runbook is multi-module (core-first tag, submodule `go.mod` bump, 12 tags per version). Decide the target version before releasing; it is not part of this plan.
