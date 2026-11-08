[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string] $ReportPath,
    [string] $ReviewPath = (Join-Path $PSScriptRoot '..\release\dependency-drift-review.json'),
    [Parameter(Mandatory)]
    [string] $RunUrl,
    # docs/releases.md freshness policy: validate and promote within seven days.
    [int] $PromotionTargetDays = 7
)

# Keeps one open issue for drift the scheduled freshness check finds at or past
# the promotion target, or past the hard fail: creates it, rewrites its body on
# each run, and closes it on the first run with no such drift. Needs `gh` with
# GH_TOKEN and GH_REPO set.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

$title = 'Dependency freshness: drift at or past the promotion target'
$report = Get-Content -Raw -LiteralPath $ReportPath | ConvertFrom-Json -DateKind String
if ($report.schemaVersion -ne 1) {
    throw "Unsupported dependency drift report schema: $($report.schemaVersion)"
}
$held = @((Get-Content -Raw -LiteralPath $ReviewPath | ConvertFrom-Json).held | ForEach-Object dependency)

$rows = @($report.drift | ForEach-Object {
    $status = if ($_.critical) { 'hard fail (critical)' } elseif ($_.hardFail) { 'hard fail' } elseif ($_.ageDays -ge $PromotionTargetDays) { 'past target' } else { 'within target' }
    [pscustomobject]@{ drift = $_; status = $status; actionable = $status -ne 'within target' }
} | Sort-Object @{ Expression = { $_.drift.hardFail }; Descending = $true }, @{ Expression = { $_.drift.ageDays }; Descending = $true }, @{ Expression = { $_.drift.dependency } })
$actionable = @($rows | Where-Object actionable)

$existing = @(gh issue list --state open --label automated --limit 100 --json number,title |
    ConvertFrom-Json | Where-Object title -eq $title | Sort-Object number)
$number = if ($existing.Count -gt 0) { $existing[0].number } else { $null }

if ($actionable.Count -eq 0) {
    if ($number) {
        gh issue close $number --comment "The scheduled check found no drift at or past the $PromotionTargetDays-day promotion target: $RunUrl"
        Write-Output "Closed issue #$number."
    } else {
        Write-Output 'No drift at or past the promotion target; no issue to close.'
    }
    return
}

function Format-Cell([object] $Value) {
    "$Value".Replace('|', '\|') -replace '[\r\n]+', ' '
}

$gate = if ($report.hardFail) { 'fails' } else { 'passes' }
$lines = [Collections.Generic.List[string]]::new()
$lines.Add("The scheduled dependency freshness check found $($actionable.Count) of $($rows.Count) drift rows at or past the $PromotionTargetDays-day promotion target. A release tag pushed now $gate the release freshness gate, which hard-fails at $($report.hardFailAfterDays) days or on a critical or integrity change and permanently reserves the tag.")
$lines.Add('')
$lines.Add("Checked at $($report.checkedAt) by $RunUrl.")
$lines.Add('')
$lines.Add('| Dependency | Pinned | Latest | Age (days) | Status | Reviewed hold | Source |')
$lines.Add('| --- | --- | --- | --- | --- | --- | --- |')
foreach ($row in $rows) {
    $drift = $row.drift
    $hold = if ($held -contains $drift.dependency) { 'yes' } else { 'no' }
    $cells = @($drift.dependency, $drift.pinned, $drift.latest, $drift.ageDays, $row.status, $hold, $drift.source) |
        ForEach-Object { Format-Cell $_ }
    $lines.Add("| $($cells -join ' | ') |")
}
$lines.Add('')
$lines.Add('Bump each row past the target, or record a reviewed hold, in `release/dependency-drift-review.json` as [docs/releases.md](https://github.com/{0}/blob/main/docs/releases.md#freshness-policy) describes. A reviewed hold keeps this issue open and still hard-fails the release once it ages past the limit. The next scheduled run rewrites this body and closes the issue once no row is at or past the target.' -f $env:GH_REPO)
$body = ($lines -join "`n") + "`n"

$bodyFile = Join-Path ([IO.Path]::GetTempPath()) "dependency-drift-issue-$PID.md"
[IO.File]::WriteAllText($bodyFile, $body, [Text.UTF8Encoding]::new($false))
try {
    if ($number) {
        gh issue edit $number --body-file $bodyFile | Out-Null
        Write-Output "Updated issue #$number."
    } else {
        gh issue create --title $title --body-file $bodyFile --label automated --label dependencies
    }
} finally {
    Remove-Item -LiteralPath $bodyFile -Force
}
