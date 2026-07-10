[CmdletBinding()]
param(
    [string] $RepositoryRoot = (Split-Path -Parent $PSScriptRoot)
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$dependencies = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot 'release\dependencies.json') | ConvertFrom-Json
$dockerfile = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot 'Dockerfile')
$freshnessMonitor = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot 'scripts\Test-DependencyFreshness.ps1')

# Freshness and immutability are separate checks. Version drift must age from
# the first unadopted release/commit, while the artifacts and release tags for
# every currently pinned version are re-resolved on every run—even when a newer
# release is already pending.
foreach ($fragment in @(
        'Get-FirstUnadoptedReleaseDate',
        'Get-ReleaseForVersion',
        'repository-pin-ancestry:',
        'compare/$($repositoryPin.commit)...$($latest.sha)?per_page=1&page=1',
        '$pinnedRunnerRelease',
        '$pinnedScaleSetRelease',
        '$pinnedActionRelease',
        '$dependencies.runner.image',
        '$dependencies.syft.image')) {
    if (-not $freshnessMonitor.Contains($fragment)) {
        throw "Dependency freshness monitor is missing a fail-closed integrity/aging contract: $fragment"
    }
}

foreach ($dependency in @($dependencies.buildx, $dependencies.buildKit, $dependencies.buildKitSbomScanner)) {
    if ($dependency.version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' -or
        $dependency.source -notmatch '^https://github\.com/') {
        throw "Invalid reviewed Docker builder dependency: $($dependency | ConvertTo-Json -Compress)"
    }
}
if ($dependencies.buildx.linuxAmd64Sha256 -notmatch '^[0-9a-f]{64}$') {
    throw 'Buildx linux/amd64 asset must have an exact lowercase SHA-256 pin'
}
foreach ($dependency in @($dependencies.buildKit, $dependencies.buildKitSbomScanner)) {
    if ($dependency.digest -notmatch '^sha256:[0-9a-f]{64}$' -or
        $dependency.linuxAmd64Digest -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Builder image dependency must pin index and linux/amd64 digests: $($dependency.image)"
    }
}

$expectedFrom = "FROM $($dependencies.runner.image)@$($dependencies.runner.digest)"
if (-not $dockerfile.Contains($expectedFrom)) {
    throw "Dockerfile must contain the pinned official runner image: $expectedFrom"
}

$expectedPowerShellVersion = "ARG POWERSHELL_VERSION=$($dependencies.powerShell.version)"
$expectedPowerShellDigest = "ARG POWERSHELL_SHA256=$($dependencies.powerShell.linuxX64ArchiveSha256)"
foreach ($expected in @($expectedPowerShellVersion, $expectedPowerShellDigest)) {
    if (-not $dockerfile.Contains($expected)) {
        throw "Dockerfile dependency pin disagrees with release/dependencies.json: $expected"
    }
}

$goModPath = Join-Path $RepositoryRoot 'go.mod'
if (Test-Path -LiteralPath $goModPath) {
    $goMod = Get-Content -Raw -LiteralPath $goModPath
    $scaleSetPattern = [regex]::Escape('github.com/actions/scaleset') + '\s+v' + [regex]::Escape($dependencies.scaleSetClient.version)
    if ($goMod -notmatch $scaleSetPattern) {
        throw "go.mod must pin github.com/actions/scaleset v$($dependencies.scaleSetClient.version)"
    }
    $goDirectivePattern = '(?m)^go\s+' + [regex]::Escape($dependencies.go.version) + '$'
    if ($goMod -notmatch $goDirectivePattern) {
        throw "go.mod must pin the Go directive to $($dependencies.go.version)"
    }
}

$workflowFiles = Get-ChildItem -LiteralPath (Join-Path $RepositoryRoot '.github\workflows') -Filter '*.yml' -File
$workflowText = ($workflowFiles | ForEach-Object { Get-Content -Raw -LiteralPath $_.FullName }) -join "`n"
$buildxInstaller = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot 'scripts\install-verified-buildx.sh')
$requiredInstallerFragments = @(
    '.buildx.version',
    '.buildx.linuxAmd64Sha256',
    'sha256sum --check --strict',
    'docker buildx version',
    'https://github.com/docker/buildx/releases/download/'
)
foreach ($fragment in $requiredInstallerFragments) {
    if (-not $buildxInstaller.Contains($fragment)) {
        throw "Verified Buildx installer is missing required behavior: $fragment"
    }
}

$buildKitReference = "$($dependencies.buildKit.image)@$($dependencies.buildKit.digest)"
$scannerReference = "$($dependencies.buildKitSbomScanner.image)@$($dependencies.buildKitSbomScanner.digest)"
$compatibilityGenerator = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot 'scripts\New-CompatibilityManifest.ps1')
$compatibilityFields = @(
    'buildxVersion',
    'buildxLinuxAmd64Sha256',
    'buildKitVersion',
    'buildKitDigest',
    'buildKitLinuxAmd64Digest',
    'sbomGeneratorVersion',
    'sbomGeneratorDigest',
    'sbomGeneratorLinuxAmd64Digest'
)
foreach ($field in $compatibilityFields) {
    if (-not $compatibilityGenerator.Contains("$field =")) {
        throw "Compatibility manifest generator does not bind reviewed builder evidence: $field"
    }
}
foreach ($required in @(
        "image=$buildKitReference",
        "sbom: generator=$scannerReference",
        'cache-binary: false',
        'buildkitd-flags: --debug=false',
        'queue: max',
        'CONTENTS.sha256',
        'gh release verify ',
        'gh release verify-asset ',
        'release validate --manifest',
        'gh release view "$RELEASE_VERSION" --repo "$GITHUB_REPOSITORY" --json assets',
        'release-transaction.cjs reconcile')) {
    if (-not $workflowText.Contains($required)) {
        throw "Workflow release contract is missing: $required"
    }
}
if ($workflowText -match '(?m)^\s*sbom:\s*true\s*$' -or
    $workflowText -match '(?i)buildkit-syft-scanner:(?:stable|latest)' -or
    $workflowText -match '(?i)moby/buildkit:(?:buildx-stable|latest|master)') {
    throw 'Workflow contains a mutable BuildKit or SBOM generator configuration'
}
$setupBuildxPattern = '(?ms)^\s{8}uses:\s*docker/setup-buildx-action@[0-9a-f]{40}[^\r\n]*\r?\n(?<block>.*?)(?=^\s{6}- name:|^\s{2}[A-Za-z0-9_-]+:|\z)'
$setupBuildxMatches = [regex]::Matches($workflowText, $setupBuildxPattern)
if ($setupBuildxMatches.Count -eq 0) {
    throw 'No pinned setup-buildx action block was found'
}
foreach ($match in $setupBuildxMatches) {
    $block = $match.Groups['block'].Value
    foreach ($required in @(
            'cache-binary: false',
            "image=$buildKitReference",
            'buildkitd-flags: --debug=false')) {
        if (-not $block.Contains($required)) {
            throw "Every setup-buildx block must use the reviewed verified-builder contract: missing $required"
        }
    }
    if ($block -match '(?m)^\s*version:') {
        throw 'setup-buildx must not download or execute a version before checksum verification'
    }
}
$releaseWorkflow = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot '.github\workflows\release.yml')
$releaseTransaction = Get-Content -Raw -LiteralPath (Join-Path $RepositoryRoot '.github\scripts\release-transaction.cjs')
foreach ($fragment in @(
        'transactionMarker(sourceSHA)',
        'more than one release exists for the requested tag',
        'await api.deleteAsset(actual.id)',
        'await assertRemoteTag(api, input)',
        'make_latest: input.prerelease ? "false" : "legacy"')) {
    if (-not $releaseTransaction.Contains($fragment)) {
        throw "Resumable release transaction is missing a fail-closed contract: $fragment"
    }
}
$candidateIndex = $releaseWorkflow.IndexOf('Build and push untagged worker candidate', [StringComparison]::Ordinal)
$publishIndex = $releaseWorkflow.IndexOf('Publish GitHub release before OCI tag promotion', [StringComparison]::Ordinal)
$promoteIndex = $releaseWorkflow.IndexOf('promote-image:', [StringComparison]::Ordinal)
if ($candidateIndex -lt 0 -or $publishIndex -le $candidateIndex -or $promoteIndex -le $publishIndex) {
    throw 'Immutable GitHub release publication must precede the separate OCI promotion job'
}
if (-not $releaseWorkflow.Contains('push-by-digest=true') -or
    $releaseWorkflow.IndexOf('docker buildx imagetools create', [StringComparison]::Ordinal) -le $promoteIndex) {
    throw 'Worker publication must build by digest and promote tags only in the final promotion job'
}
$actionPins = @{}
foreach ($action in $dependencies.githubActions) {
    if ($action.repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' -or
        $action.version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$' -or
        $action.commit -notmatch '^[0-9a-f]{40}$') {
        throw "Invalid GitHub Action dependency manifest entry: $($action | ConvertTo-Json -Compress)"
    }
    if ($actionPins.ContainsKey($action.repository)) {
        throw "Duplicate GitHub Action dependency manifest entry: $($action.repository)"
    }
    $actionPins[$action.repository] = $action
}
$reusablePins = @{}
foreach ($workflow in $dependencies.reusableWorkflows) {
    if ($workflow.reference -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$' -or
        $workflow.commit -notmatch '^[0-9a-f]{40}$' -or
        $workflow.source -ne "https://github.com/$($workflow.reference.Replace('/.github/workflows/', '/blob/' + $workflow.commit + '/.github/workflows/'))") {
        throw "Invalid reusable-workflow dependency manifest entry: $($workflow | ConvertTo-Json -Compress)"
    }
    if ($reusablePins.ContainsKey($workflow.reference)) {
        throw "Duplicate reusable-workflow dependency manifest entry: $($workflow.reference)"
    }
    $reusablePins[$workflow.reference] = $workflow
}
$repositoryPins = @{}
foreach ($pin in $dependencies.repositoryPins) {
    if ($pin.repository -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$' -or
        $pin.commit -notmatch '^[0-9a-f]{40}$' -or
        $pin.source -ne "https://github.com/$($pin.repository)/tree/$($pin.commit)") {
        throw "Invalid repository-wide dependency pin: $($pin | ConvertTo-Json -Compress)"
    }
    if ($repositoryPins.ContainsKey($pin.repository)) {
        throw "Duplicate repository-wide dependency pin: $($pin.repository)"
    }
    $repositoryPins[$pin.repository] = $pin
}
$observedActions = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$observedReusableWorkflows = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$observedRepositoryPins = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$toolPins = @{
    actionlint = "github.com/rhysd/actionlint/cmd/actionlint@v$($dependencies.actionlint.version)"
    govulncheck = "golang.org/x/vuln/cmd/govulncheck@v$($dependencies.govulncheck.version)"
    zizmor = "version: $($dependencies.zizmor.version)"
    syft = "$($dependencies.syft.image)@$($dependencies.syft.digest)"
    node = "node-version: $($dependencies.node.version)"
    buildVersion = 'github.com/melodic-software/ci-runner/internal/buildinfo.Version'
}
foreach ($tool in $toolPins.GetEnumerator()) {
    if (-not $workflowText.Contains($tool.Value)) {
        throw "Workflow $($tool.Key) pin disagrees with release/dependencies.json: $($tool.Value)"
    }
}

foreach ($workflow in $workflowFiles) {
    $content = Get-Content -Raw -LiteralPath $workflow.FullName
    foreach ($match in [regex]::Matches($content, '(?m)^\s*uses:\s*(?<reference>[^\s#]+)')) {
        $reference = $match.Groups['reference'].Value
        if ($reference.StartsWith('./')) {
            continue
        }
        if ($reference -notmatch '@[0-9a-f]{40}$') {
            throw "$($workflow.Name) contains an action or reusable workflow that is not pinned to a full commit SHA: $reference"
        }
        $parts = $reference -split '@', 2
        if ($reusablePins.ContainsKey($parts[0])) {
            if ($reusablePins[$parts[0]].commit -ne $parts[1]) {
                throw "$($workflow.Name) pin for $($parts[0]) disagrees with release/dependencies.json: $($parts[1])"
            }
            $observedReusableWorkflows.Add($parts[0]) | Out-Null
            continue
        }
        $repositoryPin = $repositoryPins.GetEnumerator() | Where-Object {
            $parts[0] -eq $_.Key -or $parts[0].StartsWith("$($_.Key)/", [StringComparison]::OrdinalIgnoreCase)
        } | Select-Object -First 1
        if ($repositoryPin) {
            if ($repositoryPin.Value.commit -ne $parts[1]) {
                throw "$($workflow.Name) pin for $($parts[0]) disagrees with repository pin $($repositoryPin.Key): $($parts[1])"
            }
            $observedRepositoryPins.Add($repositoryPin.Key) | Out-Null
            continue
        }
        if (-not $actionPins.ContainsKey($parts[0])) {
            throw "$($workflow.Name) uses $($parts[0]), which is missing from release/dependencies.json githubActions"
        }
        if ($actionPins[$parts[0]].commit -ne $parts[1]) {
            throw "$($workflow.Name) pin for $($parts[0]) disagrees with release/dependencies.json: $($parts[1])"
        }
        $observedActions.Add($parts[0]) | Out-Null
    }
    if ($content -match '(?m)^\s*runs-on:\s*ubuntu-latest\s*$') {
        throw "$($workflow.Name) uses the moving ubuntu-latest label"
    }
    if ($content -match '(?m)^\s*secrets:\s*inherit\s*$') {
        throw "$($workflow.Name) uses forbidden implicit secret inheritance"
    }
}

foreach ($repository in $actionPins.Keys) {
    if (-not $observedActions.Contains($repository)) {
        throw "Stale GitHub Action dependency manifest entry is not used by a workflow: $repository"
    }
}
foreach ($reference in $reusablePins.Keys) {
    if (-not $observedReusableWorkflows.Contains($reference)) {
        throw "Stale reusable-workflow dependency manifest entry is not used by a workflow: $reference"
    }
}
foreach ($repository in $repositoryPins.Keys) {
    if (-not $observedRepositoryPins.Contains($repository)) {
        throw "Stale repository-wide dependency pin is not used by a workflow: $repository"
    }
}

Write-Host 'Release and workflow pins are internally consistent.'
