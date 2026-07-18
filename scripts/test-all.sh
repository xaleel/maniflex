#!/usr/bin/env bash
# Builds and tests every module in the workspace.
#
# `go build ./...` / `go test ./...` do not cross module boundaries, so each
# module in go.work must be run on its own. Run from the repo root:
#
#   bash scripts/test-all.sh            # default sqlite lane (no Docker)
#   bash scripts/test-all.sh postgres   # postgres lane (testcontainers)
#
# The driver may also be set via MANIFLEX_TEST_DB; the positional arg wins.
# Only the tests module honours the driver; other modules ignore it.
set -u

driver="${1:-${MANIFLEX_TEST_DB:-sqlite}}"
export MANIFLEX_TEST_DB="$driver"
echo "Driver: $MANIFLEX_TEST_DB"

modules=(
  .
  db/postgres db/sqlite
  events/kafka events/nats events/rabbitmq events/redis
  jobs/redis middleware/auth/redis middleware/db/redis middleware/service/bcrypt
  pkg/otel
  examples tests
)

failed=()
for m in "${modules[@]}"; do
  echo "=== $m ==="
  ( cd "$m" && go build ./... ) || failed+=("$m (build)")
  ( cd "$m" && go test ./... ) || failed+=("$m (test)")
done

if [ ${#failed[@]} -ne 0 ]; then
  echo "FAILED: ${failed[*]}"
  exit 1
fi
echo "All modules passed."
