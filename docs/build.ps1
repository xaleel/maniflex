# Builds the mdBook docs, then patches the generated table-of-contents script so
# sidebar links also match the extensionless URLs that "<page>.html" redirects to.
# Run this instead of `mdbook build` directly.

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

# 1. Build
& "$root\mdbook.exe" build $root
if ($LASTEXITCODE -ne 0) { throw "mdbook build failed (exit $LASTEXITCODE)" }

# 2. Patch the generated toc-<hash>.js (hash changes with the mdBook version)
$toc = Get-ChildItem -Path "$root\book" -Filter "toc-*.js" | Select-Object -First 1
if (-not $toc) { throw "No toc-*.js found under book/ - did the build emit one?" }

$content = Get-Content -Raw -LiteralPath $toc.FullName
if ($content -match 'current_page \+ "\.html"') {
    Write-Host "Already patched: $($toc.Name)"
} else {
    # Cloudflare pages replaces trailing ".html" - add it when comparing links [for highlight]
    $content = $content -replace `
        'link\.href === current_page', `
        'link.href === current_page || link.href === current_page + ".html"'
    [System.IO.File]::WriteAllText($toc.FullName, $content, (New-Object System.Text.UTF8Encoding $false))
    Write-Host "Patched: $($toc.Name)"
}

# 3. Regenerate llms-full.txt and copy both LLM index files into the built book/.
# mdbook wipes book/ on every build, so this must run after the build above.
& "$root\build-llms-full.ps1"
if ($LASTEXITCODE -ne 0) { throw "build-llms-full.ps1 failed (exit $LASTEXITCODE)" }

foreach ($name in @("llms.txt", "llms-full.txt")) {
    $from = Join-Path $root $name
    if (-not (Test-Path $from)) { throw "Expected $name in $root but it was not found" }
    Copy-Item -LiteralPath $from -Destination (Join-Path "$root\book" $name) -Force
    Write-Host "Copied $name -> book\$name"
}
