#!/usr/bin/env bash
# Runs govulncheck against every main module declared by go.work.
#
# Run from the repository root:
#
#   bash scripts/vulncheck-all.sh
#
# The scanner is resolved at its latest version so release checks use current
# vulnerability-analysis logic and the current Go vulnerability database.
set -u

scanner="golang.org/x/vuln/cmd/govulncheck@latest"
failed=()
module_list="$(go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')" || {
  echo "Could not discover workspace modules."
  exit 1
}

while IFS='|' read -r module_path module_dir; do
  [ -n "$module_path" ] || continue
  echo "=== $module_path ==="
  (cd "$module_dir" && go run "$scanner" ./...) || failed+=("$module_path")
done <<< "$module_list"

if [ ${#failed[@]} -ne 0 ]; then
  echo "VULNERABLE: ${failed[*]}"
  exit 1
fi
echo "All workspace modules passed govulncheck."
