package sqlcore

import (
	"strings"
	"testing"

	"github.com/xaleel/maniflex"
)

// buildOrderSQL runs buildOrder and returns the ORDER BY string plus the
// collected args, without needing a real *sql.DB.
func buildOrderSQL(driver maniflex.DriverType, model *maniflex.ModelMeta, sorts []maniflex.SortExpr) (string, []any) {
	p := &ph{driver: driver}
	sql := buildOrder(model, sorts, driver, p)
	return sql, p.args
}

func localeSort(dbName, localeKey string) maniflex.SortExpr {
	return maniflex.SortExpr{DBName: dbName, IsLocale: true, LocaleKey: localeKey}
}

// A malicious locale key in ORDER BY must be threaded through as a bound
// parameter, never concatenated into the JSON-path SQL (SEC-2). This guards the
// sink even if a key reaches it without the LocaleResolver's identifier check.
const sortLocaleInjection = "en' DESC, (SELECT 1) --"

func TestBuildOrder_LocaleKeyParameterized_SQLite(t *testing.T) {
	sql, args := buildOrderSQL(maniflex.SQLite, postModel(), []maniflex.SortExpr{
		localeSort("name", sortLocaleInjection),
	})
	if want := ` ORDER BY json_extract("posts"."name", ?) ASC`; sql != want {
		t.Fatalf("locale sort key not parameterised\n got  %q\n want %q", sql, want)
	}
	if strings.Contains(sql, "DESC") || strings.Contains(sql, "SELECT") || strings.Contains(sql, sortLocaleInjection) {
		t.Fatalf("injection payload leaked into SQL: %q", sql)
	}
	if len(args) != 1 || args[0] != "$."+sortLocaleInjection {
		t.Fatalf("unexpected args (key must be bound): %v", args)
	}
}

func TestBuildOrder_LocaleKeyParameterized_Postgres(t *testing.T) {
	sql, args := buildOrderSQL(maniflex.Postgres, postModel(), []maniflex.SortExpr{
		localeSort("name", sortLocaleInjection),
	})
	if want := ` ORDER BY "posts"."name"->>$1::text ASC`; sql != want {
		t.Fatalf("locale sort key not parameterised\n got  %q\n want %q", sql, want)
	}
	if strings.Contains(sql, "DESC") || strings.Contains(sql, "SELECT") || strings.Contains(sql, sortLocaleInjection) {
		t.Fatalf("injection payload leaked into SQL: %q", sql)
	}
	if len(args) != 1 || args[0] != sortLocaleInjection {
		t.Fatalf("unexpected args (key must be bound): %v", args)
	}
}
