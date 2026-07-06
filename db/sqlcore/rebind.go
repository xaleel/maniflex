package sqlcore

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/xaleel/maniflex"
)

// rebind rewrites positional `?` placeholders to the driver's dialect. SQLite
// uses `?` natively, so its queries are returned unchanged; Postgres wants
// `$1, $2, …`. `?` characters inside single-quoted string literals are left
// untouched (a literal is not a placeholder).
func rebind(driver maniflex.DriverType, query string) string {
	if driver != maniflex.Postgres || !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inStr := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inStr:
			b.WriteByte(c)
			if c == '\'' {
				// '' is an escaped quote inside a literal — stay in-string.
				if i+1 < len(query) && query[i+1] == '\'' {
					b.WriteByte('\'')
					i++
				} else {
					inStr = false
				}
			}
		case c == '\'':
			inStr = true
			b.WriteByte(c)
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// rawKind classifies a raw SQL statement for query-vs-exec routing.
type rawKind int

const (
	rawExec      rawKind = iota // no result set — ExecContext
	rawSelect                   // SELECT / CTE-SELECT — read pool, QueryContext
	rawReturning                // data-modifying with RETURNING — write pool, QueryContext
)

var (
	reWithSelect = regexp.MustCompile(`\)\s*select`)
	reReturning  = regexp.MustCompile(`\breturning\b`)
)

// classifyRaw decides how a raw statement should be executed. It works on a
// lowercased copy with string literals and comments blanked out, so a
// "returning" appearing inside a value or a comment is not mistaken for the
// clause.
func classifyRaw(query string) rawKind {
	code := strings.TrimSpace(stripLiteralsAndComments(strings.ToLower(query)))
	if strings.HasPrefix(code, "select") ||
		(strings.HasPrefix(code, "with") && reWithSelect.MatchString(code)) {
		return rawSelect
	}
	if reReturning.MatchString(code) {
		return rawReturning
	}
	return rawExec
}

// stripLiteralsAndComments replaces single-quoted string literals, `--` line
// comments, and `/* */` block comments with a single space so keyword detection
// only sees SQL code, not string/comment contents. It does not need to preserve
// the query's executable form — the original string is what gets executed.
func stripLiteralsAndComments(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
			if i < len(q) {
				b.WriteByte('\n')
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			i += 2
			for i+1 < len(q) && !(q[i] == '*' && q[i+1] == '/') {
				i++
			}
			i++ // land on '/'; loop's i++ moves past it
			b.WriteByte(' ')
		case c == '\'':
			i++
			for i < len(q) {
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						i += 2 // escaped ''
						continue
					}
					break
				}
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
