[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidatePattern('^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$')]
    [string] $ReleaseVersion,

    [Parameter(Mandatory)]
    [ValidatePattern('^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$')]
    [string] $SourceRepository,

    [Parameter(Mandatory)]
    [ValidatePattern('^[0-9a-f]{40}$')]
    [string] $SourceSha,

    [Parameter(Mandatory)]
    [ValidateScript({ Test-Path -LiteralPath $_ -PathType Leaf })]
    [string] $ControllerArchive,

    [Parameter(Mandatory)]
    [ValidatePattern('^ghcr\.io/')]
    [string] $WorkerImage,

    [Parameter(Mandatory)]
    [ValidatePattern('^sha256:[0-9a-f]{64}$')]
    [string] $WorkerDigest,

    [Parameter(Mandatory)]
    [string] $ControllerSbom,

    [Parameter(Mandatory)]
    [string] $ControllerProvenance,

    [Parameter(Mandatory)]
    [string] $WorkerSbom,

    [Parameter(Mandatory)]
    [string] $WorkerProvenance,

    [string] $Checksums = 'SHA256SUMS',

    [string] $DependencyManifest = (Join-Path $PSScriptRoot '..\release\dependencies.json'),

    [string] $OutputPath = 'compatibility.json'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$dependencies = Get-Content -Raw -LiteralPath $DependencyManifest | ConvertFrom-Json
if ($dependencies.schemaVersion -ne 1) {
    throw "Unsupported dependency manifest schema version: $($dependencies.schemaVersion)"
}

$archive = Get-Item -LiteralPath $ControllerArchive
$archiveDigest = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive.FullName).Hash.ToLowerInvariant()

$manifest = [ordered]@{
    schemaVersion = 1
    createdAt = [DateTimeOffset]::UtcNow.ToString('o')
    releaseVersion = $ReleaseVersion
    source = [ordered]@{
        repository = $SourceRepository
        sha = $SourceSha
    }
    controller = [ordered]@{
        version = $ReleaseVersion.TrimStart('v')
        windowsArchive = $archive.Name
        archiveDigest = "sha256:$archiveDigest"
    }
    worker = [ordered]@{
        image = $WorkerImage
        digest = $WorkerDigest
    }
    dependencies = [ordered]@{
        runnerVersion = $dependencies.runner.version
        scaleSetClientVersion = $dependencies.scaleSetClient.version
        scaleSetClientCommit = $dependencies.scaleSetClient.commit
        goToolchain = "go$($dependencies.go.version)"
        powerShellVersion = $dependencies.powerShell.version
        ghVersion = $dependencies.gh.version
        ghLinuxAmd64ArchiveSha256 = $dependencies.gh.linuxAmd64ArchiveSha256
        buildxVersion = $dependencies.buildx.version
        buildxLinuxAmd64Sha256 = $dependencies.buildx.linuxAmd64Sha256
        buildKitVersion = $dependencies.buildKit.version
        buildKitDigest = $dependencies.buildKit.digest
        buildKitLinuxAmd64Digest = $dependencies.buildKit.linuxAmd64Digest
        sbomGeneratorVersion = $dependencies.buildKitSbomScanner.version
        sbomGeneratorDigest = $dependencies.buildKitSbomScanner.digest
        sbomGeneratorLinuxAmd64Digest = $dependencies.buildKitSbomScanner.linuxAmd64Digest
    }
    evidence = [ordered]@{
        checksums = $Checksums
        controllerSbom = $ControllerSbom
        controllerProvenance = $ControllerProvenance
        workerSbom = $WorkerSbom
        workerProvenance = $WorkerProvenance
    }
}

$parent = Split-Path -Parent $OutputPath
if ($parent) {
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
}

$json = $manifest | ConvertTo-Json -Depth 8
[IO.File]::WriteAllText($OutputPath, "$json`n", [Text.UTF8Encoding]::new($false))
