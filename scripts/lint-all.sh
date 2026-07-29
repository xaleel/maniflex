#!/usr/bin/env bash
# Runs go vet and Staticcheck in every module declared by go.work.
#
# Staticcheck's known findings are compared exactly with a checked-in baseline:
# existing debt remains visible, while any new finding fails CI. Removing a
# finding also fails until the baseline is deliberately reduced.
set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
baseline="$root/scripts/staticcheck-baseline.txt"
staticcheck_bin="${STATICCHECK:-staticcheck}"
actual="$(mktemp)"
trap 'rm -f "$actual"' EXIT

module_list="$(cd "$root" && go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')" || {
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
  echo "=== vet: $module_path ==="
  (cd "$module_dir" && go vet ./...) || failed+=("$module_path (vet)")

  echo "=== staticcheck: $module_path ==="
  output="$(cd "$module_dir" && "$staticcheck_bin" ./... 2>&1)"
  while IFS= read -r finding; do
    [[ -n "$finding" ]] || continue
    location="${finding%%:*}"
    finding="${location//\\//}:${finding#*:}"
    printf '%s|%s\n' "$module_path" "$finding" >> "$actual"
  done <<< "$output"
done <<< "$module_list"

LC_ALL=C sort -u -o "$actual" "$actual"
if ! diff -u <(grep -vE '^(#|$)' "$baseline" | LC_ALL=C sort) "$actual"; then
  echo "Staticcheck findings changed; fix new findings and reduce the baseline when debt is removed." >&2
  failed+=("staticcheck baseline")
fi

if [[ ${#failed[@]} -ne 0 ]]; then
  echo "FAILED: ${failed[*]}" >&2
  exit 1
fi
echo "All workspace modules passed vet and the Staticcheck baseline."
