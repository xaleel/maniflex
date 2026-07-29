package maniflextest_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/xaleel/maniflex/maniflextest"
)

func TestPostgres(t *testing.T) {
	dsn := os.Getenv("MANIFLEX_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set MANIFLEX_TEST_PG_DSN to run the PostgreSQL compatibility test")
	}

	server := maniflextest.New(t, maniflextest.Options{
		Models:   []any{Widget{}},
		Database: maniflextest.Postgres(dsn),
	})
	server.POST("/widgets", map[string]any{"name": "postgres"}).
		AssertStatus(http.StatusCreated)
}
