# Builds and tests every module in the workspace.
#
# `go build ./...` / `go test ./...` do not cross module boundaries, so each
# module in go.work must be run on its own. Run from the repo root:
#
#   pwsh scripts/test-all.ps1            # default sqlite lane (no Docker)
#   pwsh scripts/test-all.ps1 postgres   # postgres lane (testcontainers)
#
# The driver may also be set via MANIFLEX_TEST_DB; the positional arg wins.
# Only the tests module honours the driver; other modules ignore it.
param(
    [string]$Driver = ''
)
$ErrorActionPreference = 'Continue'

if (-not $Driver) {
    if ($env:MANIFLEX_TEST_DB) { $Driver = $env:MANIFLEX_TEST_DB } else { $Driver = 'sqlite' }
}
$env:MANIFLEX_TEST_DB = $Driver
Write-Host "Driver: $env:MANIFLEX_TEST_DB" -ForegroundColor Cyan

$modules = @(
    '.',
    'db/postgres', 'db/sqlite',
    'events/kafka', 'events/nats', 'events/rabbitmq', 'events/redis',
    'jobs/redis', 'middleware/db/redis', 'middleware/service/bcrypt',
    'pkg/otel',
    'examples', 'tests'
)

$failed = @()
foreach ($m in $modules) {
    Write-Host "=== $m ===" -ForegroundColor Cyan
    Push-Location $m
    go build ./...
    if ($LASTEXITCODE -ne 0) { $failed += "$m (build)" }
    go test ./...
    if ($LASTEXITCODE -ne 0) { $failed += "$m (test)" }
    Pop-Location
}

if ($failed.Count -gt 0) {
    Write-Host "FAILED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "All modules passed." -ForegroundColor Green
