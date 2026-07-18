package maniflex

// tags_unknown.go — rejection of unrecognised mfx: options (MS-2).
//
// The option switch in parseFieldTags matches exact, case-sensitive strings and
// used to drop anything else on the floor. That is a quiet failure for a
// descriptive tag (`sortable` misspelt just means no sorting), but a dangerous
// one for a protective tag: `mfx:"read_only"` leaves Readonly false, so the
// field stays client-writable and the protection the tag exists for is simply
// absent — with no error, no warning, and nothing in the OpenAPI spec to show
// it. Unknown options are now a registration error, and because every realistic
// case is a near-miss of a real option, the error says which one.

import (
	"fmt"
	"strings"
)

// knownBareOpts are the mfx options matched by their exact, whole spelling —
// those that stand alone with no value, and those that carry one from a fixed set
// ("auto_delete:false", "upload:presigned"). The latter belong here rather than in
// knownPrefixOpts because the value is part of what is known: mfx:"upload:x" is
// not a valid option written badly, it is not an option, and listing the bare
// prefix would both claim otherwise and cost the better suggestion — "upload:foo"
// is one edit from "upload:presigned" and offers it.
//
// Kept in sync with the switch in parseFieldTags — an option added there must be
// added here or it will be rejected as unknown, which the round-trip test
// enforces.
var knownBareOpts = []string{
	"auto_delete:false",
	"dynamic",
	"encrypted",
	"file",
	"filterable",
	"hidden",
	"immutable",
	"index",
	"locale",
	"norelation",
	"readonly",
	"relation",
	"required",
	"resolve",
	"scheduled",
	"searchable",
	"sortable",
	"split",
	"unique",
	"upload:presigned",
	"upload:stream",
	"writeonly",
}

// knownPrefixOpts are the mfx options that carry a value after their prefix.
var knownPrefixOpts = []string{
	"accept:",
	"cursor_field:",
	"default:",
	"default_locale:",
	"enum:",
	"file_acl:",
	"key:",
	"lock_scope:",
	"lock_when:",
	"max:",
	"max_count:",
	"max_size:",
	"min:",
	"relation:",
	"scheduled;",
	"through:",
}

// suggestOpt returns the known option `opt` most plausibly meant, or "" when
// nothing is close enough to be worth guessing. A wrong-case spelling is an
// exact match once folded, so it is checked first and always wins; otherwise the
// nearest option within a small edit distance is offered. The distance budget
// scales with length so short options don't collect wild suggestions ("min" is
// two edits from "max", and suggesting it would be worse than saying nothing).
func suggestOpt(opt string) string {
	if opt == "" {
		return ""
	}
	lower := strings.ToLower(opt)

	// A value-carrying option is only ever compared on its key, so `Enum:a|b`
	// suggests `enum:` rather than being measured against the whole literal.
	key, isPrefixed := optKey(opt)
	if isPrefixed {
		lowerKey := strings.ToLower(key)
		for _, k := range knownPrefixOpts {
			if lowerKey == k {
				return k
			}
		}
		if s := nearest(lowerKey, knownPrefixOpts); s != "" {
			return s
		}
		// The key matches no free-value prefix, but a value-constrained option is
		// known by its whole spelling and lives in the bare list — so measure the
		// whole literal there before giving up. Without this, every misspelling of
		// one ("upload:presined", "auto_delete:flase") is told only that it is
		// unknown, though the option it means is a single edit away.
		return nearest(lower, knownBareOpts)
	}

	for _, k := range knownBareOpts {
		if lower == k {
			return k
		}
	}

	// A value-carrying option written without its value — `mfx:"lock_scope"`
	// instead of `mfx:"lock_scope:StockBalance"`. Point at the real option
	// rather than measure the bare key against unrelated candidates.
	for _, k := range knownPrefixOpts {
		if lower+string(k[len(k)-1]) == k {
			return k
		}
	}
	return nearest(lower, knownBareOpts)
}

// optKey splits a value-carrying option into its key ("enum:") and reports
// whether it had one. Bare options report false.
func optKey(opt string) (string, bool) {
	if i := strings.IndexAny(opt, ":;"); i >= 0 {
		return opt[:i+1], true
	}
	return opt, false
}

// nearest returns the candidate within the edit-distance budget for opt's
// length, or "" if none qualifies or if several tie. A tie means we do not know
// which was meant, and a confident wrong guess is worse than none: `mix:` is one
// edit from both `min:` and `max:`, which mean opposite things, so naming either
// would send the reader to change something that was not the problem.
func nearest(opt string, candidates []string) string {
	budget := 1
	if len(opt) >= 5 {
		budget = 2 // "reaodnly" -> "readonly" is two edits
	}

	// opt already carries its ":"/";" when prefixed (optKey keeps it), and the
	// candidates in that list carry theirs, so the two are directly comparable.
	var best []string
	bestDist := budget + 1
	for _, c := range candidates {
		d := editDistance(opt, c)
		switch {
		case d < bestDist:
			bestDist, best = d, []string{c}
		case d == bestDist:
			best = append(best, c)
		}
	}
	if bestDist > budget || len(best) != 1 {
		return "" // nothing close enough, or too close to call
	}
	return best[0]
}

// editDistance is Levenshtein distance — the count of single-character inserts,
// deletions and substitutions between a and b. It covers the shapes a mistyped
// tag actually takes: a stray separator (`read_only`), a dropped letter, and a
// transposition (`reaodnly`, two edits).
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// unknownOptError builds the registration error for a field carrying options the
// parser does not recognise.
func unknownOptError(model, field string, opts []string) error {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		if s := suggestOpt(o); s != "" {
			parts = append(parts, fmt.Sprintf("%q (did you mean %q?)", o, s))
			continue
		}
		parts = append(parts, fmt.Sprintf("%q", o))
	}
	return fmt.Errorf(
		"maniflex: model %q field %q has unknown mfx option %s — an unrecognised option is "+
			"not applied, so a protective directive that is misspelt leaves the field unprotected",
		model, field, strings.Join(parts, ", "))
}
