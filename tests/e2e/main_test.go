package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xaleel/maniflex/tests/e2e/testutil"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestMain wires up the database backend for the whole e2e package.
//
//   - SQLite lane (default): nothing to provision — each test opens its own
//     in-memory database. No Docker required.
//   - Postgres lane (MANIFLEX_TEST_DB=postgres / -db=postgres): either use the
//     DSN from MANIFLEX_TEST_PG_DSN (for CI that already runs a Postgres
//     service, or hosts without Docker-in-Docker), or start one testcontainers
//     Postgres instance for the whole binary. Per-test schema isolation
//     (testutil.openPostgres) lives inside this single instance, so the
//     container is paid for once per package run, not per test.
func TestMain(m *testing.M) {
	// Parse flags now so testutil.Driver() can read -db before m.Run().
	flag.Parse()

	if !testutil.IsPostgres() {
		os.Exit(m.Run())
	}

	// Postgres lane: prefer an externally supplied DSN.
	if dsn := strings.TrimSpace(os.Getenv("MANIFLEX_TEST_PG_DSN")); dsn != "" {
		testutil.SetPostgresDSN(dsn)
		os.Exit(m.Run())
	}

	// No override DSN → bring our own Postgres via testcontainers.
	dsn, terminate, err := startPostgresContainer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: start postgres container: %v\n", err)
		os.Exit(1)
	}
	testutil.SetPostgresDSN(dsn)

	code := m.Run()
	terminate()
	os.Exit(code)
}

// startPostgresContainer launches a single Postgres container for the test
// binary and returns its DSN plus a terminate func.
func startPostgresContainer() (string, func(), error) {
	ctx := context.Background()

	// tcpostgres.Run applies a default wait strategy that waits for the
	// "ready to accept connections" log twice plus a successful query, which is
	// more robust than a bare port check; we only extend its deadline.
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("maniflex_test"),
		tcpostgres.WithUsername("maniflex"),
		tcpostgres.WithPassword("maniflex"),
		testcontainers.WithWaitStrategy(
			wait.ForAll(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithDeadline(120*time.Second),
		),
	)
	if err != nil {
		return "", nil, err
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(ctx)
		return "", nil, fmt.Errorf("connection string: %w", err)
	}

	terminate := func() {
		_ = container.Terminate(context.Background())
	}
	return dsn, terminate, nil
}
