# Builds the mdBook docs, then patches the generated table-of-contents script so
# sidebar links also match the extensionless URLs that "<page>.html" redirects to.
# It also mirrors the raw source Markdown into book/ so every page is reachable as
# both "<link>.html" (rendered) and "<link>.md" (source), and emits a sitemap.xml.
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

# 4. Mirror the raw source Markdown into book/ alongside each rendered page, so
# every page is reachable as both "<link>.html" (mdBook) and "<link>.md" (source).
# mdbook renders src\<path>.md -> book\<path>.html but does not copy the .md itself.
# Must run after the build, which wipes book/ each time.
$src = Join-Path $root "src"
$book = Join-Path $root "book"
$mdCount = 0
Get-ChildItem -Path $src -Recurse -File -Filter "*.md" | ForEach-Object {
    $rel = $_.FullName.Substring($src.Length).TrimStart('\', '/')
    $dest = Join-Path $book $rel
    $destDir = Split-Path -Parent $dest
    if (-not (Test-Path $destDir)) { New-Item -ItemType Directory -Path $destDir -Force | Out-Null }
    Copy-Item -LiteralPath $_.FullName -Destination $dest -Force
    $mdCount++
}
Write-Host "Copied $mdCount source .md file(s) -> book\ (raw Markdown alongside .html)"

# 5. Generate sitemap.xml from the source pages. URLs mirror mdBook's output
# (file-path based, extensionless), so they match the deployed site. The base host
# is read from book.toml's `cname`. index.md pages map to their directory URL
# (root index -> "/", nested <dir>/index -> "/<dir>/").
$cname = if ((Get-Content -Raw -LiteralPath (Join-Path $root "book.toml")) -match '(?m)^\s*cname\s*=\s*"([^"]+)"') {
    $matches[1]
} else {
    "docs.maniflex.dev"
}
$baseUrl = "https://$cname"

$sb = [System.Text.StringBuilder]::new()
[void]$sb.AppendLine('<?xml version="1.0" encoding="UTF-8"?>')
[void]$sb.AppendLine('<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">')

$urlCount = 0
Get-ChildItem -Path $src -Recurse -File -Filter "*.md" |
    Where-Object { $_.Name -ne "SUMMARY.md" } |
    Sort-Object FullName |
    ForEach-Object {
        $path = $_.FullName.Substring($src.Length).TrimStart('\', '/').Replace('\', '/') -replace '\.md$', ''
        if ($path -eq 'index') {
            $loc = "$baseUrl/"                                   # site root
        } elseif ($path -like '*/index') {
            $loc = "$baseUrl/" + ($path -replace 'index$', '')   # nested directory index -> "/<dir>/"
        } else {
            $loc = "$baseUrl/$path"
        }
        $loc = $loc.Replace('&', '&amp;')
        $lastmod = $_.LastWriteTimeUtc.ToString('yyyy-MM-dd')
        [void]$sb.AppendLine("  <url><loc>$loc</loc><lastmod>$lastmod</lastmod></url>")
        $urlCount++
    }

[void]$sb.AppendLine('</urlset>')
[System.IO.File]::WriteAllText((Join-Path $book "sitemap.xml"), $sb.ToString(), (New-Object System.Text.UTF8Encoding $false))
Write-Host "Wrote sitemap.xml ($urlCount URLs) -> book\sitemap.xml"
