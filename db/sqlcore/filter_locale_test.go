package sqlcore

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// localeF builds a locale filter expression (?filter=<field>.<key>:op:val).
func localeF(field, localeKey string, op maniflex.FilterOperator, val any) *maniflex.FilterExpr {
	return &maniflex.FilterExpr{
		Field:     field,
		IsLocale:  true,
		LocaleKey: localeKey,
		Operator:  op,
		Value:     val,
		Group:     -1,
	}
}

// A malicious locale key must be threaded through the query as a bound
// parameter, never concatenated into the JSON-path SQL (SEC-1). This is the
// defence-in-depth half of the fix — it guards the SQL sink even if a key ever
// reaches it without going through the parse-time allowlist.
const localeInjection = "ar') OR 1=1 --"

func TestFilterConds_LocaleKeyParameterized_SQLite(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.SQLite, postModel(), []*maniflex.FilterExpr{
		localeF("name", localeInjection, maniflex.OpEq, "x"),
	})

	// The key is a bound placeholder inside json_extract's path argument.
	if want := `json_extract("posts"."name", ?) = ?`; sql != want {
		t.Fatalf("locale key not parameterised\n got  %q\n want %q", sql, want)
	}
	// The raw injection must not appear anywhere in the SQL string.
	if strings.Contains(sql, "OR 1=1") || strings.Contains(sql, localeInjection) {
		t.Fatalf("injection payload leaked into SQL: %q", sql)
	}
	// The key travels as data: the bound path, then the filter value.
	if len(args) != 2 || args[0] != "$."+localeInjection || args[1] != "x" {
		t.Fatalf("unexpected args (key must be bound before value): %v", args)
	}
}

func TestFilterConds_LocaleKeyParameterized_Postgres(t *testing.T) {
	sql, args := filterCondsSQL(maniflex.Postgres, postModel(), []*maniflex.FilterExpr{
		localeF("name", localeInjection, maniflex.OpEq, "x"),
	})

	if want := `"posts"."name"->>$1::text = $2`; sql != want {
		t.Fatalf("locale key not parameterised\n got  %q\n want %q", sql, want)
	}
	if strings.Contains(sql, "OR 1=1") || strings.Contains(sql, localeInjection) {
		t.Fatalf("injection payload leaked into SQL: %q", sql)
	}
	if len(args) != 2 || args[0] != localeInjection || args[1] != "x" {
		t.Fatalf("unexpected args (key must be bound before value): %v", args)
	}
}
