package sqlcore

// like/ilike hand the value to SQL as a pattern: % and _ in it are wildcards, and
// with no ESCAPE clause there is no portable way to mean them literally (SQLite has
// no escape character by default, Postgres has a backslash — so the same filter
// behaves differently on the two). contains/starts_with/ends_with escape the value
// and spell the ESCAPE out, so a search for "50%" finds "50%" on both (BUG-22).

import (
	"testing"

	"github.com/xaleel/maniflex"
)

func TestFilterConds_Contains_EscapesWildcards(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpContains, "50%", -1),
	})

	want := `LOWER("posts"."status") LIKE LOWER(?) ESCAPE '\'`
	if sql != want {
		t.Errorf("sql:\n got %q\nwant %q", sql, want)
	}
	if len(args) != 1 || args[0] != `%50\%%` {
		t.Errorf(`args: got %v, want [%%50\%%%%] — the value's %% must be escaped, the surrounding ones not`, args)
	}
}

func TestFilterConds_Contains_Postgres(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.Postgres, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpContains, "a_b", -1),
	})

	want := `"posts"."status" ILIKE $1 ESCAPE '\'`
	if sql != want {
		t.Errorf("sql:\n got %q\nwant %q", sql, want)
	}
	if len(args) != 1 || args[0] != `%a\_b%` {
		t.Errorf(`args: got %v, want [%%a\_b%%] — the underscore must not stay a single-char wildcard`, args)
	}
}

func TestFilterConds_StartsWith_And_EndsWith(t *testing.T) {
	cases := []struct {
		op   maniflex.FilterOperator
		want string
	}{
		{maniflex.OpStartsWith, `AB\_%`},
		{maniflex.OpEndsWith, `%AB\_`},
	}
	for _, c := range cases {
		_, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
			f("status", c.op, "AB_", -1),
		})
		if len(args) != 1 || args[0] != c.want {
			t.Errorf("%s: args got %v, want [%s]", c.op, args, c.want)
		}
	}
}

// The escape character itself is a metacharacter once ESCAPE is in play: a value
// containing a backslash must not turn the character after it into an escape.
func TestLikePattern_EscapesTheEscapeCharacter(t *testing.T) {
	got := maniflex.LikePattern(maniflex.OpContains, `c:\dir%`)
	want := `%c:\\dir\%%`
	if got != want {
		t.Errorf("LikePattern: got %q, want %q", got, want)
	}
}

// like/ilike are unchanged: the value is still a raw pattern, with no ESCAPE.
func TestFilterConds_Like_StillARawPattern(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		f("status", maniflex.OpLike, "50%", -1),
	})
	if want := `"posts"."status" LIKE ?`; sql != want {
		t.Errorf("sql: got %q, want %q", sql, want)
	}
	if len(args) != 1 || args[0] != "50%" {
		t.Errorf("args: got %v, want [50%%] — like must pass the pattern through untouched", args)
	}
}
