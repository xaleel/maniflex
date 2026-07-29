#!/usr/bin/env bash
# Verifies every publishable workspace module independently of go.work.
# examples and tests are repository-only modules and are the sole exclusions.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_list="$(cd "$root" && go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')"
if [[ -z "$module_list" ]]; then
  echo "Could not discover workspace modules." >&2
  exit 1
fi

while IFS='|' read -r module_path module_dir; do
  [[ -n "$module_path" ]] || continue
  case "$module_path" in
    github.com/xaleel/maniflex/examples|github.com/xaleel/maniflex/tests)
      echo "=== skip non-release module: $module_path ==="
      continue
      ;;
  esac

  echo "=== release smoke: $module_path ==="
  (
    cd "$module_dir"
    temp_mod="$(mktemp .release-smoke-XXXXXX.mod)"
    temp_sum="${temp_mod%.mod}.sum"
    trap 'rm -f "$temp_mod" "$temp_sum"' EXIT
    cp go.mod "$temp_mod"
    if [[ -f go.sum ]]; then
      cp go.sum "$temp_sum"
    fi

    GOWORK=off go mod download -modfile="$temp_mod"
    GOWORK=off go mod verify -modfile="$temp_mod"
    GOWORK=off go build -modfile="$temp_mod" ./...

    # Core's repository tests intentionally import the nested SQLite module
    # through go.work. Tidying core outside the workspace would incorrectly
    # turn that test-only adapter into a public core dependency.
    if [[ "$module_path" == "github.com/xaleel/maniflex" ]]; then
      exit 0
    fi

    GOWORK=off go test -modfile="$temp_mod" ./...
  )
done <<< "$module_list"

echo "Every publishable workspace module passed the release dry-run."
