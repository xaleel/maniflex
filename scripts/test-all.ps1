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

$moduleLines = @(go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')
if ($LASTEXITCODE -ne 0 -or $moduleLines.Count -eq 0) {
    Write-Host 'Could not discover workspace modules.' -ForegroundColor Red
    exit 1
}

$failed = @()
foreach ($line in $moduleLines) {
    if (-not $line) {
        continue
    }
    $modulePath, $moduleDir = $line -split '\|', 2
    Write-Host "=== $modulePath ===" -ForegroundColor Cyan
    Push-Location $moduleDir
    go build ./...
    if ($LASTEXITCODE -ne 0) { $failed += "$modulePath (build)" }
    go test ./...
    if ($LASTEXITCODE -ne 0) { $failed += "$modulePath (test)" }
    Pop-Location
}

if ($failed.Count -gt 0) {
    Write-Host "FAILED: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host "All modules passed." -ForegroundColor Green
