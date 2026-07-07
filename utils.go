package maniflex

import (
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"
)

// toSnakeCase converts CamelCase to snake_case.
//
//	"AuthorID"  → "author_id"
//	"CreatedAt" → "created_at"
//	"UserRoleID"→ "user_role_id"
func toSnakeCase(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			if unicode.IsLower(prev) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(prev)) {
				b.WriteRune('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// pluralize applies basic English pluralisation rules to a snake_case word.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)

	irregulars := map[string]string{
		"person": "people", "child": "children",
		"man": "men", "woman": "women",
		"mouse": "mice", "goose": "geese",
	}
	if p, ok := irregulars[lower]; ok {
		return p
	}

	switch {
	case strings.HasSuffix(lower, "s"),
		strings.HasSuffix(lower, "x"),
		strings.HasSuffix(lower, "z"),
		strings.HasSuffix(lower, "ch"),
		strings.HasSuffix(lower, "sh"):
		return s + "es"
	case strings.HasSuffix(lower, "y") && len(s) > 1 &&
		!strings.ContainsRune("aeiou", rune(lower[len(lower)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

// tableNameFromModelName derives the default DB table name from a Go struct name.
//
//	"Post"     → "posts"
//	"UserRole" → "user_roles"
func tableNameFromModelName(name string) string {
	return pluralize(toSnakeCase(name))
}

// SplitCSV splits a comma-separated string, trimming whitespace from each part.
// Exported so DB adapters in sub-packages can use it.
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

const UPPER = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
const LOWER = "abcdefghijklmnopqrstuvwxyz"
const DIGITS = "0123456789"
const UPPER_D = UPPER + DIGITS
const LOWER_D = LOWER + DIGITS
const ALPHANUM = UPPER + LOWER + DIGITS

// RandomString returns a cryptographically-secure random string of the given
// length, each character drawn uniformly (without modulo bias) from charset.
// It is safe for tokens, session IDs, and other secrets. charset is indexed by
// byte, so pass an ASCII charset such as ALPHANUM.
//
// A non-positive length or an empty charset returns "". It panics only if the
// operating system's secure random source fails, which does not occur in normal
// operation.
func RandomString(length int, charset string) string {
	if length <= 0 || len(charset) == 0 {
		return ""
	}
	n := big.NewInt(int64(len(charset)))
	out := make([]byte, length)
	for i := range out {
		idx, err := crand.Int(crand.Reader, n)
		if err != nil {
			panic("maniflex.RandomString: crypto/rand failed: " + err.Error())
		}
		out[i] = charset[idx.Int64()]
	}
	return string(out)
}

// scanSQLRows scans all rows from a *sql.Rows into column-keyed maps.
// Used by ServerContext.RawQuery when falling back to the adapter path.
func scanSQLRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func FormatDuration(d time.Duration) string {
	nanos := float64(d.Nanoseconds())

	switch {
	case nanos < 1000:
		return fmt.Sprintf("%gns", nanos)
	case nanos < 1000000:
		return fmt.Sprintf("%gμs", nanos/1000)
	case nanos < 1000000000:
		return fmt.Sprintf("%gms", nanos/1000000)
	default:
		return fmt.Sprintf("%gs", nanos/1000000000)
	}
}
