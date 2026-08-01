#!/usr/bin/env bash
# Measures framework coverage across BOTH modules that exercise it and enforces
# floors on the merged result.
#
# The root module holds the unit tests; tests/ holds the end-to-end suite. Each
# `go test` run writes its own profile, so measuring the root module alone —
# which is what the coverage gate used to do — reports 0% for packages whose
# entire suite is end-to-end (middleware/idempotency, middleware/db,
# jobs/maniflex, events/outbox, middleware/service were all 0% by that
# measure, and all are above 75% in truth). Both runs are pointed at the same
# -coverpkg set so their profiles describe the same statements and can be merged.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${COVERAGE_OUTDIR:-$root}"

# Aggregate floor, and floors for packages big or sensitive enough that the
# aggregate could hide a regression in them. Raise these as coverage improves;
# they are minimums, not targets.
FLOOR="${COVERAGE_FLOOR:-80}"
PKG_FLOORS="${COVERAGE_PKG_FLOORS:-\
github.com/xaleel/maniflex=80,\
github.com/xaleel/maniflex/db/sqlcore=75,\
github.com/xaleel/maniflex/middleware/auth=72,\
github.com/xaleel/maniflex/realtime=85}"

# The measurement target: every package in the root module. Satellite modules
# (db/sqlite, storage/s3, admin, ...) carry their own tests and are out of scope
# here, matching what the readiness audit measured.
pkgs="$(cd "$root" && go list ./... | paste -sd, -)"

echo "=== unit coverage (root module) ==="
(cd "$root" && go test -covermode=atomic -coverpkg="$pkgs" -coverprofile="$outdir/coverage-unit.out" ./...)

echo "=== end-to-end coverage (tests module) ==="
# -coverpkg names packages this module does not itself contain, so go warns
# about any that the e2e suite never imports. That is expected, not an error.
(cd "$root/tests" && go test -covermode=atomic -coverpkg="$pkgs" -coverprofile="$outdir/coverage-e2e.out" ./...)

echo "=== merged ==="
(cd "$root" && go run ./internal/cmd/covmerge \
  -out "$outdir/coverage.out" \
  -floor "$FLOOR" \
  -pkg-floor "$PKG_FLOORS" \
  -v \
  "$outdir/coverage-unit.out" "$outdir/coverage-e2e.out")
