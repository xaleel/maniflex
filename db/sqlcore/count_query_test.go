package sqlcore

// The list total used to be COUNT(DISTINCT <table>.id). Every join a list query can
// carry is 1:1 — a nested filter/sort joins a BelongsTo relation on its primary key
// (any other kind is rejected before it reaches SQL) and the SQLite FTS join matches
// one shadow row per base row on rowid — so no join fans the base table out and the
// DISTINCT only forced a sort/hash over the whole filtered set, defeating an
// index-only count. It is now COUNT(*) (PERF-1).
//
// The e2e suite pins the numbers this must keep producing; these pin the SQL, which
// is where the cost lives and the only place the change is visible.

import (
	"strings"
	"testing"
)

func TestCountQuerySQL_UsesPlainCountStar(t *testing.T) {
	got := countQuerySQL("orders", "", "")

	if strings.Contains(strings.ToUpper(got), "DISTINCT") {
		t.Errorf("count query still carries a DISTINCT — it forces a sort/hash over the "+
			"whole filtered set, and no join a list query can carry fans out:\n  %s", got)
	}
	if !strings.Contains(got, "COUNT(*)") {
		t.Errorf("want a plain COUNT(*), got:\n  %s", got)
	}
}

// The DISTINCT is what was dropped — the JOIN and WHERE it counted over must still
// be there, or the total silently counts the wrong set.
func TestCountQuerySQL_KeepsJoinsAndWhere(t *testing.T) {
	join := ` LEFT JOIN "users" AS "user" ON "posts"."user_id" = "user"."id"`
	where := ` WHERE "user"."role" = $1`

	got := countQuerySQL("posts", join, where)

	for _, want := range []string{`COUNT(*)`, `FROM "posts"`, join, where} {
		if !strings.Contains(got, want) {
			t.Errorf("count query lost %q:\n  %s", want, got)
		}
	}
	if strings.Contains(strings.ToUpper(got), "DISTINCT") {
		t.Errorf("count query reintroduced DISTINCT:\n  %s", got)
	}
}
