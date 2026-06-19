# Generates llms-full.txt by concatenating every docs page (in SUMMARY.md order)
# behind the same header used in llms.txt. Run after editing docs content.

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$src = Join-Path $root "src"
$out = Join-Path $root "llms-full.txt"

# Reuse the H1 + blockquote summary from llms.txt (read as UTF-8 to preserve em-dashes).
$llms = Get-Content -Raw -Encoding UTF8 -LiteralPath (Join-Path $root "llms.txt")
$intro = ($llms -split "`n## ")[0].TrimEnd()  # everything before the first "## " section

$note = "This file contains the full text of the maniflex documentation, concatenated for use by LLMs. See https://docs.maniflex.dev/ for the rendered version and https://docs.maniflex.dev/llms.txt for a links-only index."

# Pull doc links from SUMMARY.md in order: [Title](path.md)
$summary = Get-Content -Raw -Encoding UTF8 -LiteralPath (Join-Path $src "SUMMARY.md")
$matches = [regex]::Matches($summary, '\[(?<title>[^\]]+)\]\((?<path>[^)]+\.md)\)')

$sb = [System.Text.StringBuilder]::new()
[void]$sb.Append($intro)
[void]$sb.Append("`n`n")
[void]$sb.Append($note)
[void]$sb.Append("`n")

$seen = @{}
foreach ($m in $matches) {
    $rel = $m.Groups['path'].Value
    if ($seen.ContainsKey($rel)) { continue }
    $seen[$rel] = $true

    $file = Join-Path $src ($rel -replace '/', '\')
    if (-not (Test-Path $file)) {
        Write-Warning "Missing: $rel"
        continue
    }

    $body = (Get-Content -Raw -Encoding UTF8 -LiteralPath $file).TrimEnd()
    [void]$sb.Append("`n`n---`n`n")
    [void]$sb.Append($body)
    [void]$sb.Append("`n")
}

$utf8NoBom = New-Object System.Text.UTF8Encoding $false
[System.IO.File]::WriteAllText($out, $sb.ToString(), $utf8NoBom)
Write-Host "Wrote $out ($($seen.Count) pages)"
