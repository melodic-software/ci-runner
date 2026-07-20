[CmdletBinding()]
param(
    [string] $DependencyManifest = (Join-Path $PSScriptRoot '..\release\dependencies.json'),
    [string] $OutputPath = 'dependency-drift.json',
    [int] $HardFailAfterDays = 14
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$headers = @{
    Accept = 'application/vnd.github+json'
    'X-GitHub-Api-Version' = '2026-03-10'
    'User-Agent' = 'melodic-software-ci-runner-dependency-monitor'
}
if ($env:GITHUB_TOKEN) {
    $headers.Authorization = "Bearer $($env:GITHUB_TOKEN)"
}
$dependencies = Get-Content -Raw -LiteralPath $DependencyManifest | ConvertFrom-Json
$now = [DateTimeOffset]::UtcNow
$drift = [Collections.Generic.List[object]]::new()

function Get-GitHubReleases([string] $Repository) {
    $items = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$Repository/releases?per_page=100"
    foreach ($release in $items) {
        if (-not $release.draft -and -not $release.prerelease) {
            Write-Output $release
        }
    }
}

function Get-LatestRelease([object[]] $Releases) {
    $Releases | Sort-Object { [version]$_.tag_name.TrimStart('v') } -Descending | Select-Object -First 1
}

function Get-FirstUnadoptedReleaseDate([object[]] $Releases, [string] $PinnedVersion) {
    $pinned = [version]$PinnedVersion
    $pending = @($Releases | Where-Object { [version]$_.tag_name.TrimStart('v') -gt $pinned })
    if ($pending.Count -eq 0) {
        return $null
    }
    [DateTimeOffset](($pending | Sort-Object { [DateTimeOffset]$_.published_at } | Select-Object -First 1).published_at)
}

function Get-ReleaseForVersion([object[]] $Releases, [string] $Version, [string] $Repository) {
    $releaseMatches = @($Releases | Where-Object { $_.tag_name.TrimStart('v') -eq $Version })
    if ($releaseMatches.Count -ne 1) {
        throw "Official repository $Repository does not have exactly one release for pinned version $Version"
    }
    $releaseMatches[0]
}

function Get-GitHubTagCommit([string] $Repository, [string] $Tag) {
    $reference = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$Repository/git/ref/tags/$Tag"
    $object = $reference.object
    for ($depth = 0; $depth -lt 10; $depth++) {
        if ($object.type -eq 'commit' -and $object.sha -match '^[0-9a-f]{40}$') {
            return $object.sha
        }
        if ($object.type -ne 'tag' -or $object.sha -notmatch '^[0-9a-f]{40}$') {
            throw "Unsupported GitHub tag object for $Repository@$Tag`: $($object.type) $($object.sha)"
        }
        $tagObject = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$Repository/git/tags/$($object.sha)"
        $object = $tagObject.object
    }
    throw "GitHub tag nesting exceeds the supported depth for $Repository@$Tag"
}

function Get-OfficialImageIndexDigest([string] $Reference) {
    $inspection = & docker buildx imagetools inspect $Reference 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to inspect official image $Reference`: $inspection"
    }
    $match = [regex]::Match(($inspection -join "`n"), '(?m)^Digest:\s+(sha256:[0-9a-f]{64})$')
    if (-not $match.Success) {
        throw "Official image inspection did not return an index digest: $Reference"
    }
    $match.Groups[1].Value
}

function Get-OfficialLinuxAmd64Digest([string] $Reference) {
    $raw = & docker buildx imagetools inspect $Reference --raw 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to inspect official image manifest $Reference`: $raw"
    }
    $index = ($raw -join "`n") | ConvertFrom-Json
    $platformManifests = @($index.manifests | Where-Object {
        $_.platform.os -eq 'linux' -and $_.platform.architecture -eq 'amd64'
    })
    if ($platformManifests.Count -ne 1 -or $platformManifests[0].digest -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Official image does not have exactly one linux/amd64 manifest: $Reference"
    }
    $platformManifests[0].digest
}

function Add-Drift(
    [string] $Name,
    [string] $Pinned,
    [string] $Latest,
    [DateTimeOffset] $PublishedAt,
    [string] $Source,
    [bool] $Critical = $false
) {
    if ($Pinned -eq $Latest) {
        return
    }

    $ageDays = [Math]::Max(0, [Math]::Floor(($now - $PublishedAt).TotalDays))
    $drift.Add([ordered]@{
        dependency = $Name
        pinned = $Pinned
        latest = $Latest
        publishedAt = $PublishedAt.ToString('o')
        ageDays = $ageDays
        critical = $Critical
        hardFail = $Critical -or $ageDays -ge $HardFailAfterDays
        source = $Source
    })
}

function Add-IntegrityDrift(
    [string] $Name,
    [string] $Pinned,
    [string] $Observed,
    [DateTimeOffset] $PublishedAt,
    [string] $Source
) {
    if ($Pinned -eq $Observed) {
        return
    }
    $drift.Add([ordered]@{
        dependency = $Name
        pinned = $Pinned
        latest = $Observed
        publishedAt = $PublishedAt.ToString('o')
        ageDays = 0
        critical = $true
        hardFail = $true
        source = $Source
    })
}

$runnerReleases = Get-GitHubReleases 'actions/runner'
$runner = Get-LatestRelease $runnerReleases
$runnerPendingDate = Get-FirstUnadoptedReleaseDate $runnerReleases $dependencies.runner.version
$runnerCritical = @($runnerReleases | Where-Object {
    [version]$_.tag_name.TrimStart('v') -gt [version]$dependencies.runner.version -and
    $_.body -match '(?i)\bcritical\b|\bsecurity\b|CVE-[0-9]{4}-[0-9]+'
}).Count -gt 0
$runnerDriftDate = if ($runnerPendingDate) { $runnerPendingDate } else { [DateTimeOffset]$runner.published_at }
Add-Drift 'actions-runner' $dependencies.runner.version $runner.tag_name.TrimStart('v') $runnerDriftDate $runner.html_url $runnerCritical

$pinnedRunnerRelease = Get-ReleaseForVersion $runnerReleases $dependencies.runner.version 'actions/runner'
$runnerAssetName = "actions-runner-linux-x64-$($dependencies.runner.version).tar.gz"
$runnerAsset = $pinnedRunnerRelease.assets | Where-Object { $_.name -eq $runnerAssetName }
if (-not $runnerAsset -or $runnerAsset.digest -notmatch '^sha256:[0-9a-f]{64}$') {
    throw "Official runner release is missing a checksummed $runnerAssetName asset"
}
Add-IntegrityDrift 'actions-runner-archive-digest' "sha256:$($dependencies.runner.archiveSha256)" `
    $runnerAsset.digest ([DateTimeOffset]$pinnedRunnerRelease.published_at) $runnerAsset.browser_download_url

$scaleSetReleases = Get-GitHubReleases 'actions/scaleset'
$scaleSet = Get-LatestRelease $scaleSetReleases
$scaleSetPendingDate = Get-FirstUnadoptedReleaseDate $scaleSetReleases $dependencies.scaleSetClient.version
$scaleSetDriftDate = if ($scaleSetPendingDate) { $scaleSetPendingDate } else { [DateTimeOffset]$scaleSet.published_at }
Add-Drift 'actions-scaleset' $dependencies.scaleSetClient.version $scaleSet.tag_name.TrimStart('v') $scaleSetDriftDate $scaleSet.html_url
$pinnedScaleSetRelease = Get-ReleaseForVersion $scaleSetReleases $dependencies.scaleSetClient.version 'actions/scaleset'
$scaleSetCommit = Get-GitHubTagCommit 'actions/scaleset' $pinnedScaleSetRelease.tag_name
Add-IntegrityDrift 'actions-scaleset-tag-commit' $dependencies.scaleSetClient.commit $scaleSetCommit `
    ([DateTimeOffset]$pinnedScaleSetRelease.published_at) $pinnedScaleSetRelease.html_url

foreach ($builder in @(
    @{ Name = 'docker-buildx'; Repository = 'docker/buildx'; Manifest = $dependencies.buildx; Image = $null },
    @{ Name = 'moby-buildkit'; Repository = 'moby/buildkit'; Manifest = $dependencies.buildKit; Image = 'moby/buildkit' },
    @{ Name = 'buildkit-syft-scanner'; Repository = 'docker/buildkit-syft-scanner'; Manifest = $dependencies.buildKitSbomScanner; Image = 'docker/buildkit-syft-scanner' }
)) {
    $releases = Get-GitHubReleases $builder.Repository
    $semanticReleases = @($releases |
        Where-Object { $_.tag_name -match '^v?[0-9]+\.[0-9]+\.[0-9]+$' })
    $latest = Get-LatestRelease $semanticReleases
    if (-not $latest) {
        throw "Official builder repository has no stable semantic release: $($builder.Repository)"
    }
    $pendingDate = Get-FirstUnadoptedReleaseDate $semanticReleases $builder.Manifest.version
    $driftDate = if ($pendingDate) { $pendingDate } else { [DateTimeOffset]$latest.published_at }
    Add-Drift $builder.Name $builder.Manifest.version $latest.tag_name.TrimStart('v') $driftDate $latest.html_url
    $pinnedRelease = Get-ReleaseForVersion $semanticReleases $builder.Manifest.version $builder.Repository

    if ($builder.Name -eq 'docker-buildx') {
        $assetName = "buildx-v$($builder.Manifest.version).linux-amd64"
        $asset = $pinnedRelease.assets | Where-Object name -eq $assetName
        if (-not $asset -or $asset.digest -notmatch '^sha256:[0-9a-f]{64}$') {
            throw "Official Buildx release is missing a checksummed $assetName asset"
        }
        Add-IntegrityDrift 'docker-buildx-linux-amd64-digest' `
            "sha256:$($builder.Manifest.linuxAmd64Sha256)" $asset.digest `
            ([DateTimeOffset]$pinnedRelease.published_at) $asset.browser_download_url
    } elseif ($builder.Image) {
        $reference = $builder.Manifest.image
        $digest = Get-OfficialImageIndexDigest $reference
        $platformDigest = Get-OfficialLinuxAmd64Digest $reference
        Add-IntegrityDrift "$($builder.Name)-image-digest" `
            "$($builder.Manifest.digest) / $($builder.Manifest.linuxAmd64Digest)" `
            "$digest / $platformDigest" ([DateTimeOffset]$pinnedRelease.published_at) $pinnedRelease.html_url
    }
}

foreach ($repositoryPin in $dependencies.repositoryPins) {
    $repository = $repositoryPin.repository
    $repo = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$repository"
    $latest = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/$repository/commits/$($repo.default_branch)"
    $comparison = Invoke-RestMethod -Headers $headers -Uri (
        "https://api.github.com/repos/$repository/compare/$($repositoryPin.commit)...$($latest.sha)?per_page=1&page=1"
    )
    if ($comparison.status -eq 'identical') {
        continue
    }
    if ($comparison.status -ne 'ahead' -or $comparison.total_commits -lt 1 -or @($comparison.commits).Count -ne 1) {
        Add-IntegrityDrift "repository-pin-ancestry:$repository" $repositoryPin.commit $latest.sha `
            ([DateTimeOffset]$latest.commit.committer.date) $repositoryPin.source
        continue
    }
    $firstUnadoptedDate = [DateTimeOffset]$comparison.commits[0].commit.committer.date
    Add-Drift "repository-pin:$repository" $repositoryPin.commit $latest.sha `
        $firstUnadoptedDate $repositoryPin.source
}

foreach ($action in $dependencies.githubActions) {
    $actionReleases = Get-GitHubReleases $action.repository
    $semanticActionReleases = @($actionReleases |
        Where-Object { $_.tag_name -match '^v?[0-9]+\.[0-9]+\.[0-9]+$' })
    $latestAction = Get-LatestRelease $semanticActionReleases
    if (-not $latestAction) {
        throw "Official GitHub Action repository has no stable semantic release: $($action.repository)"
    }
    $pendingDate = Get-FirstUnadoptedReleaseDate $semanticActionReleases $action.version
    $driftDate = if ($pendingDate) { $pendingDate } else { [DateTimeOffset]$latestAction.published_at }
    Add-Drift "github-action:$($action.repository)" $action.version $latestAction.tag_name.TrimStart('v') $driftDate $latestAction.html_url

    $pinnedActionRelease = Get-ReleaseForVersion $semanticActionReleases $action.version $action.repository
    $pinnedTagCommit = Get-GitHubTagCommit $action.repository $pinnedActionRelease.tag_name
    Add-IntegrityDrift "github-action-tag:$($action.repository)" $action.commit $pinnedTagCommit `
        ([DateTimeOffset]$pinnedActionRelease.published_at) $pinnedActionRelease.html_url
}

foreach ($tool in @(
    @{ Name = 'actionlint'; Repository = 'rhysd/actionlint'; Pinned = $dependencies.actionlint.version },
    @{ Name = 'zizmor'; Repository = 'zizmorcore/zizmor'; Pinned = $dependencies.zizmor.version }
)) {
    $releases = Get-GitHubReleases $tool.Repository
    $latest = Get-LatestRelease $releases
    $pendingDate = Get-FirstUnadoptedReleaseDate $releases $tool.Pinned
    $driftDate = if ($pendingDate) { $pendingDate } else { [DateTimeOffset]$latest.published_at }
    Add-Drift $tool.Name $tool.Pinned $latest.tag_name.TrimStart('v') $driftDate $latest.html_url
}

$syftReleases = Get-GitHubReleases 'anchore/syft'
$syft = Get-LatestRelease $syftReleases
$syftPendingDate = Get-FirstUnadoptedReleaseDate $syftReleases $dependencies.syft.version
$syftDriftDate = if ($syftPendingDate) { $syftPendingDate } else { [DateTimeOffset]$syft.published_at }
Add-Drift 'syft' $dependencies.syft.version $syft.tag_name.TrimStart('v') $syftDriftDate $syft.html_url
$pinnedSyftRelease = Get-ReleaseForVersion $syftReleases $dependencies.syft.version 'anchore/syft'
$pinnedSyftImage = $dependencies.syft.image
$syftInspect = & docker buildx imagetools inspect $pinnedSyftImage 2>&1
if ($LASTEXITCODE -ne 0) {
    throw "Unable to inspect official Syft image $pinnedSyftImage`: $syftInspect"
}
$syftDigestMatch = [regex]::Match(($syftInspect -join "`n"), '(?m)^Digest:\s+(sha256:[0-9a-f]{64})$')
if (-not $syftDigestMatch.Success) {
    throw "Official Syft image inspection did not return an index digest: $pinnedSyftImage"
}
$observedSyftDigest = $syftDigestMatch.Groups[1].Value
Add-IntegrityDrift 'syft-image-digest' $dependencies.syft.digest $observedSyftDigest `
    ([DateTimeOffset]$pinnedSyftRelease.published_at) 'https://github.com/anchore/syft/pkgs/container/syft'

$vulnVersionsResponse = Invoke-WebRequest -Uri 'https://proxy.golang.org/golang.org/x/vuln/@v/list'
$vulnVersions = @($vulnVersionsResponse.Content -split "`n" | Where-Object {
    $_ -match '^v[0-9]+\.[0-9]+\.[0-9]+$'
})
$latestVulnVersion = $vulnVersions | Sort-Object { [version]$_.TrimStart('v') } -Descending | Select-Object -First 1
$pendingVulnVersions = @($vulnVersions | Where-Object {
    [version]$_.TrimStart('v') -gt [version]$dependencies.govulncheck.version
})
$vulnInfo = Invoke-RestMethod -Uri "https://proxy.golang.org/golang.org/x/vuln/@v/$latestVulnVersion.info"
$vulnDriftDate = [DateTimeOffset]$vulnInfo.Time
if ($pendingVulnVersions.Count -gt 0) {
    $vulnDates = foreach ($version in $pendingVulnVersions) {
        $info = Invoke-RestMethod -Uri "https://proxy.golang.org/golang.org/x/vuln/@v/$version.info"
        [DateTimeOffset]$info.Time
    }
    $vulnDriftDate = $vulnDates | Sort-Object | Select-Object -First 1
}
Add-Drift 'govulncheck' $dependencies.govulncheck.version $latestVulnVersion.TrimStart('v') $vulnDriftDate "https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck@$latestVulnVersion"

$hostedManifestCommits = Invoke-RestMethod -Headers $headers -Uri 'https://api.github.com/repos/actions/runner-images/commits?path=images/ubuntu/Ubuntu2404-Readme.md&per_page=1'
$hostedManifestCommit = $hostedManifestCommits[0]
$hostedManifestSha = $hostedManifestCommit.sha
$hostedManifestUrl = "https://raw.githubusercontent.com/actions/runner-images/$hostedManifestSha/images/ubuntu/Ubuntu2404-Readme.md"
$hostedManifest = Invoke-RestMethod -Uri $hostedManifestUrl
$hostedPowerShellMatch = [regex]::Match($hostedManifest, '(?m)^- PowerShell ([0-9]+\.[0-9]+\.[0-9]+)\s*$')
if (-not $hostedPowerShellMatch.Success) {
    throw "Official Ubuntu 24.04 hosted-image manifest does not contain a parseable PowerShell version: $hostedManifestSha"
}
$hostedPowerShellVersion = $hostedPowerShellMatch.Groups[1].Value
$powerShell = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/PowerShell/PowerShell/releases/tags/v$hostedPowerShellVersion"
$powerShellReleases = Get-GitHubReleases 'PowerShell/PowerShell'
$eligiblePowerShellReleases = @($powerShellReleases | Where-Object {
    $_.tag_name -match '^v[0-9]+\.[0-9]+\.[0-9]+$' -and
    [version]$_.tag_name.TrimStart('v') -le [version]$hostedPowerShellVersion
})
$powerShellPendingDate = Get-FirstUnadoptedReleaseDate $eligiblePowerShellReleases $dependencies.powerShell.version
$powerShellDriftDate = if ($powerShellPendingDate) { $powerShellPendingDate } else { [DateTimeOffset]$powerShell.published_at }
Add-Drift 'github-hosted-powershell' $dependencies.powerShell.version $hostedPowerShellVersion $powerShellDriftDate "https://github.com/actions/runner-images/blob/$hostedManifestSha/images/ubuntu/Ubuntu2404-Readme.md"
$pinnedPowerShellRelease = Get-ReleaseForVersion $powerShellReleases $dependencies.powerShell.version 'PowerShell/PowerShell'
$powerShellAssetName = "powershell-$($dependencies.powerShell.version)-linux-x64.tar.gz"
$powerShellAsset = $pinnedPowerShellRelease.assets | Where-Object { $_.name -eq $powerShellAssetName }
if (-not $powerShellAsset -or $powerShellAsset.digest -notmatch '^sha256:[0-9a-f]{64}$') {
    throw "Official PowerShell release is missing a checksummed $powerShellAssetName asset"
}
Add-IntegrityDrift 'powershell-archive-digest' "sha256:$($dependencies.powerShell.linuxX64ArchiveSha256)" `
    $powerShellAsset.digest ([DateTimeOffset]$pinnedPowerShellRelease.published_at) $powerShellAsset.browser_download_url

$hostedGhMatch = [regex]::Match($hostedManifest, '(?m)^- GitHub CLI ([0-9]+\.[0-9]+\.[0-9]+)\s*$')
if (-not $hostedGhMatch.Success) {
    throw "Official Ubuntu 24.04 hosted-image manifest does not contain a parseable GitHub CLI version: $hostedManifestSha"
}
$hostedGhVersion = $hostedGhMatch.Groups[1].Value
$gh = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/cli/cli/releases/tags/v$hostedGhVersion"
$ghReleases = Get-GitHubReleases 'cli/cli'
$eligibleGhReleases = @($ghReleases | Where-Object {
    $_.tag_name -match '^v[0-9]+\.[0-9]+\.[0-9]+$' -and
    [version]$_.tag_name.TrimStart('v') -le [version]$hostedGhVersion
})
$ghPendingDate = Get-FirstUnadoptedReleaseDate $eligibleGhReleases $dependencies.gh.version
$ghDriftDate = if ($ghPendingDate) { $ghPendingDate } else { [DateTimeOffset]$gh.published_at }
Add-Drift 'github-hosted-gh' $dependencies.gh.version $hostedGhVersion $ghDriftDate "https://github.com/actions/runner-images/blob/$hostedManifestSha/images/ubuntu/Ubuntu2404-Readme.md"
$pinnedGhRelease = Get-ReleaseForVersion $ghReleases $dependencies.gh.version 'cli/cli'
$ghAssetName = "gh_$($dependencies.gh.version)_linux_amd64.tar.gz"
$ghAsset = $pinnedGhRelease.assets | Where-Object { $_.name -eq $ghAssetName }
if (-not $ghAsset -or $ghAsset.digest -notmatch '^sha256:[0-9a-f]{64}$') {
    throw "Official GitHub CLI release is missing a checksummed $ghAssetName asset"
}
Add-IntegrityDrift 'gh-archive-digest' "sha256:$($dependencies.gh.linuxAmd64ArchiveSha256)" `
    $ghAsset.digest ([DateTimeOffset]$pinnedGhRelease.published_at) $ghAsset.browser_download_url

$nodeReleases = Invoke-RestMethod -Uri 'https://nodejs.org/dist/index.json'
$nodeLtsReleases = @($nodeReleases | Where-Object { $_.lts -and $_.lts -ne $false })
$node = $nodeLtsReleases |
    Sort-Object { [version]$_.version.TrimStart('v') } -Descending |
    Select-Object -First 1
if (-not $node) {
    throw 'The official Node.js release index contains no LTS release'
}
$pendingNodeReleases = @($nodeLtsReleases | Where-Object {
    [version]$_.version.TrimStart('v') -gt [version]$dependencies.node.version
})
$nodeDriftRelease = if ($pendingNodeReleases.Count -gt 0) {
    $pendingNodeReleases | Sort-Object { [DateTimeOffset]::Parse("$($_.date)T00:00:00Z") } | Select-Object -First 1
} else {
    $node
}
$nodeDriftDate = [DateTimeOffset]::Parse("$($nodeDriftRelease.date)T00:00:00Z")
Add-Drift 'node-lts' $dependencies.node.version $node.version.TrimStart('v') $nodeDriftDate "https://nodejs.org/dist/$($node.version)/"

$goReleases = Invoke-RestMethod -Uri 'https://go.dev/dl/?mode=json'
$go = $goReleases | Where-Object { $_.stable } | Select-Object -First 1
$goCommit = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/golang/go/commits/$($go.version)"
$goTags = Invoke-RestMethod -Headers $headers -Uri 'https://api.github.com/repos/golang/go/tags?per_page=100'
$pendingGoTags = @($goTags | Where-Object {
    $_.name -match '^go[0-9]+\.[0-9]+\.[0-9]+$' -and
    [version]$_.name.TrimStart('go') -gt [version]$dependencies.go.version -and
    [version]$_.name.TrimStart('go') -le [version]$go.version.TrimStart('go')
})
$goDriftDate = [DateTimeOffset]$goCommit.commit.committer.date
if ($pendingGoTags.Count -gt 0) {
    $pendingGoDates = foreach ($tag in $pendingGoTags) {
        $commit = Invoke-RestMethod -Headers $headers -Uri "https://api.github.com/repos/golang/go/commits/$($tag.name)"
        [DateTimeOffset]$commit.commit.committer.date
    }
    $goDriftDate = $pendingGoDates | Sort-Object | Select-Object -First 1
}
Add-Drift 'go' $dependencies.go.version $go.version.TrimStart('go') $goDriftDate 'https://go.dev/dl/'

$runnerImage = $dependencies.runner.image
$inspect = & docker buildx imagetools inspect $runnerImage 2>&1
if ($LASTEXITCODE -ne 0) {
    throw "Unable to inspect official runner image $runnerImage`: $inspect"
}
$digestMatch = [regex]::Match(($inspect -join "`n"), '(?m)^Digest:\s+(sha256:[0-9a-f]{64})$')
if (-not $digestMatch.Success) {
    throw "Official runner image inspection did not return an index digest: $runnerImage"
}
$observedRunnerDigest = $digestMatch.Groups[1].Value
Add-IntegrityDrift 'actions-runner-image-digest' $dependencies.runner.digest $observedRunnerDigest `
    ([DateTimeOffset]$pinnedRunnerRelease.published_at) 'https://github.com/actions/actions-runner/pkgs/container/actions-runner'

$report = [ordered]@{
    schemaVersion = 1
    checkedAt = $now.ToString('o')
    hardFailAfterDays = $HardFailAfterDays
    hasDrift = $drift.Count -gt 0
    hardFail = @($drift | Where-Object hardFail).Count -gt 0
    drift = $drift
}

$parent = Split-Path -Parent $OutputPath
if ($parent) {
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
}
[IO.File]::WriteAllText($OutputPath, "$(($report | ConvertTo-Json -Depth 8))`n", [Text.UTF8Encoding]::new($false))

$report | ConvertTo-Json -Depth 8
