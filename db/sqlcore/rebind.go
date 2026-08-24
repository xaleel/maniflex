package sqlcore

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/xaleel/maniflex"
)

// rebind rewrites positional `?` placeholders to the driver's dialect. SQLite
// uses `?` natively, so its queries are returned unchanged; Postgres wants
// `$1, $2, …`. A `?` outside executable code is left alone: it is text, not a
// placeholder.
//
// "Outside executable code" used to mean only "inside a single-quoted literal",
// which left a `?` in a comment, a quoted identifier, a dollar-quoted body or an
// E-string being rewritten. That is worse than a mangled literal, because every
// real placeholder after it shifts by one and the statement then asks for more
// parameters than were supplied (audit O7).
//
// `?|` and `?&` are preserved: they are jsonb operators and can never be
// placeholders. A bare `?` is genuinely ambiguous — jsonb's key-exists operator
// is spelt the same as a placeholder — and is still rewritten, so a raw query
// needing that operator cannot be expressed. See scanSQL.
func rebind(driver maniflex.DriverType, query string) string {
	if driver != maniflex.Postgres || !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for _, seg := range scanSQL(query) {
		if !seg.isCode {
			b.WriteString(seg.text)
			continue
		}
		for i := 0; i < len(seg.text); i++ {
			c := seg.text[i]
			if c != '?' {
				b.WriteByte(c)
				continue
			}
			if i+1 < len(seg.text) && (seg.text[i+1] == '|' || seg.text[i+1] == '&') {
				b.WriteByte(c) // jsonb ?| / ?&
				continue
			}
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		}
	}
	return b.String()
}

// sqlSegment is a run of a statement that is either executable code or a region
// where SQL syntax does not apply — a string literal, a quoted identifier, or a
// comment.
type sqlSegment struct {
	text   string
	isCode bool
}

// scanSQL splits q into alternating code and non-code runs.
//
// One scanner serves both callers here. They had one each, and the two disagreed
// about what SQL looks like: stripLiteralsAndComments understood comments and
// rebind did not, in the same file. Two descriptions of one grammar drift, and
// the one that drifted was the one nothing tested.
//
// It is a lexer, not a parser: it knows where code is, never what the code
// means. An unterminated literal or comment runs to the end of the input, which
// the database rejects on its own.
func scanSQL(q string) []sqlSegment {
	var segs []sqlSegment
	codeStart := 0
	flushCode := func(to int) {
		if to > codeStart {
			segs = append(segs, sqlSegment{text: q[codeStart:to], isCode: true})
		}
	}
	for i := 0; i < len(q); {
		var end int
		switch {
		case q[i] == '\'':
			end = scanQuoted(q, i, '\'', isEStringQuote(q, i))
		case q[i] == '"':
			end = scanQuoted(q, i, '"', false)
		case q[i] == '-' && i+1 < len(q) && q[i+1] == '-':
			end = scanLineComment(q, i)
		case q[i] == '/' && i+1 < len(q) && q[i+1] == '*':
			end = scanBlockComment(q, i)
		case q[i] == '$':
			e, ok := scanDollarQuoted(q, i)
			if !ok {
				i++
				continue
			}
			end = e
		default:
			i++
			continue
		}
		flushCode(i)
		segs = append(segs, sqlSegment{text: q[i:end], isCode: false})
		i = end
		codeStart = end
	}
	flushCode(len(q))
	return segs
}

// scanQuoted returns the index just past the closing quote. A doubled quote is
// an escape and keeps the literal open. backslashEscapes additionally consumes
// the byte after a backslash, which is E-string behaviour only.
func scanQuoted(q string, i int, quote byte, backslashEscapes bool) int {
	for i++; i < len(q); i++ {
		if backslashEscapes && q[i] == '\\' && i+1 < len(q) {
			i++
			continue
		}
		if q[i] != quote {
			continue
		}
		if i+1 < len(q) && q[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(q)
}

// isEStringQuote reports whether the quote at i opens an E'…' literal, where a
// backslash escapes the next character. A plain literal leaves backslashes
// alone under standard_conforming_strings, on by default since Postgres 9.1 —
// so treating every literal as backslash-escaped would be just as wrong.
//
// The E has to begin a token; the trailing e of an identifier does not count.
func isEStringQuote(q string, i int) bool {
	if i == 0 || (q[i-1] != 'E' && q[i-1] != 'e') {
		return false
	}
	return i-1 == 0 || !isIdentByte(q[i-2])
}

func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}

// scanLineComment returns the index of the terminating newline, which is left as
// code so the statement keeps its line structure.
func scanLineComment(q string, i int) int {
	for i < len(q) && q[i] != '\n' {
		i++
	}
	return i
}

// scanBlockComment handles nesting, which Postgres supports: in
// /* a /* b */ c */ the first terminator closes the inner comment only.
func scanBlockComment(q string, i int) int {
	depth := 0
	for i < len(q) {
		switch {
		case q[i] == '/' && i+1 < len(q) && q[i+1] == '*':
			depth++
			i += 2
		case q[i] == '*' && i+1 < len(q) && q[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(q)
}

// scanDollarQuoted returns the index just past the closing $tag$, and whether i
// opens one at all.
//
// An opener needs a closing $ immediately after its tag, so a $1 placeholder is
// not one: the character after its digits is whatever follows in the statement.
// An opener with no matching closer is not a dollar quote either — it is a lone
// $ in the code.
//
// Postgres also forbids a tag starting with a digit. That is not checked,
// because reaching it needs the shape $1$…$1$, which no valid statement has —
// and a guard no input can exercise is one that only looks like protection.
func scanDollarQuoted(q string, i int) (int, bool) {
	j := i + 1
	for j < len(q) && isIdentByte(q[j]) {
		j++
	}
	if j >= len(q) || q[j] != '$' {
		return 0, false
	}
	tag := q[i : j+1]
	k := strings.Index(q[j+1:], tag)
	if k < 0 {
		return 0, false
	}
	return j + 1 + k + len(tag), true
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

// stripLiteralsAndComments replaces every non-code region — string literals,
// quoted identifiers, dollar-quoted bodies and comments — with a single space,
// so keyword detection sees SQL code and not the contents of a value someone
// wrote. It does not need to preserve the query's executable form; the original
// string is what gets executed.
//
// It reads the same scan rebind does, so a region one of them respects cannot be
// a region the other rewrites.
func stripLiteralsAndComments(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	for _, seg := range scanSQL(q) {
		if seg.isCode {
			b.WriteString(seg.text)
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}
