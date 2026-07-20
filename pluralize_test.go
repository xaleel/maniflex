package maniflex

// 11D.4 — pluralize on Latin/Greek stems.
//
// The suffix rules are English-only, so a classical stem got an English ending:
// "matrix" ends in x → "matrixes", "analysis" ends in s → "analysises", and
// "datum" matched nothing → "datums". The output is a table name and an m2m
// relation key, so it is visible both in the schema and in the API.
//
// Fixed with an explicit word list rather than new suffix rules. A blanket
// "-um → -a" turns album into alba and a blanket "-ex → -ices" turns complex
// into complices; the words below are enumerated precisely because their
// morphology cannot be inferred from their spelling.

import "testing"

func TestPluralize(t *testing.T) {
	cases := []struct{ in, want string }{
		// The three the audit named.
		{"matrix", "matrices"},
		{"datum", "data"},
		{"analysis", "analyses"},

		// Same families, same confidence.
		{"vertex", "vertices"},
		{"criterion", "criteria"},
		{"phenomenon", "phenomena"},
		{"medium", "media"},
		{"axis", "axes"},
		{"basis", "bases"},
		{"diagnosis", "diagnoses"},
		{"thesis", "theses"},
		{"quiz", "quizzes"}, // the -z rule gave "quizes"

		// ── Must not change ──────────────────────────────────────────────
		// English words whose endings look classical but are not. Each of
		// these is what a naive morphological rule would break.
		{"album", "albums"},     // not "alba"
		{"complex", "complexes"}, // not "complices"
		{"forum", "forums"},     // "fora" is archaic
		{"box", "boxes"},
		{"bus", "buses"},
		{"status", "statuses"},
		{"index", "indexes"}, // "indices" is not the DB-world plural

		// The pre-existing rules must be untouched.
		{"post", "posts"},
		{"category", "categories"},
		{"day", "days"},
		{"church", "churches"},
		{"dish", "dishes"},
		{"person", "people"},
		{"child", "children"},
		{"", ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := pluralize(tc.in); got != tc.want {
				t.Errorf("pluralize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTableNameFromModelName_ClassicalStems: pluralize's output is a table name,
// so this is the surface the change is actually visible on.
func TestTableNameFromModelName_ClassicalStems(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Matrix", "matrices"},
		{"Analysis", "analyses"},
		{"Datum", "data"},
		{"BloodAnalysis", "blood_analyses"},
		{"Post", "posts"},
		{"UserRole", "user_roles"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := tableNameFromModelName(tc.in); got != tc.want {
				t.Errorf("tableNameFromModelName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
