# Runs govulncheck against every main module declared by go.work.
#
# Run from the repository root:
#
#   pwsh scripts/vulncheck-all.ps1
#
# The scanner is resolved at its latest version so release checks use current
# vulnerability-analysis logic and the current Go vulnerability database.
$ErrorActionPreference = 'Stop'

$scanner = 'golang.org/x/vuln/cmd/govulncheck@latest'
$moduleLines = @(go list -m -f '{{if .Main}}{{.Path}}|{{.Dir}}{{end}}')
if ($LASTEXITCODE -ne 0) {
    throw 'Could not discover workspace modules.'
}

$failed = @()
foreach ($line in $moduleLines) {
    if (-not $line) {
        continue
    }
    $modulePath, $moduleDir = $line -split '\|', 2
    Write-Host "=== $modulePath ===" -ForegroundColor Cyan
    Push-Location $moduleDir
    try {
        go run $scanner ./...
        if ($LASTEXITCODE -ne 0) {
            $failed += $modulePath
        }
    }
    finally {
        Pop-Location
    }
}

if ($failed.Count -gt 0) {
    Write-Host "VULNERABLE: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}
Write-Host 'All workspace modules passed govulncheck.' -ForegroundColor Green
