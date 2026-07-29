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

module_list="$(go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')" || {
  echo "Could not discover workspace modules." >&2
  exit 1
}
if [[ -z "$module_list" ]]; then
  echo "Could not discover workspace modules." >&2
  exit 1
fi

failed=()
while IFS='|' read -r module_path module_dir; do
  [[ -n "$module_path" ]] || continue
  echo "=== $module_path ==="
  ( cd "$module_dir" && go build ./... ) || failed+=("$module_path (build)")
  ( cd "$module_dir" && go test ./... ) || failed+=("$module_path (test)")
done <<< "$module_list"

if [ ${#failed[@]} -ne 0 ]; then
  echo "FAILED: ${failed[*]}"
  exit 1
fi
echo "All modules passed."
