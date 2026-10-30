# Dot-source this file. Dependabot rewrites the Dockerfile FROM line and the
# workflow `uses:` lines but cannot edit release/dependencies.json, so the runner
# image and Action pins are read from the lines it rewrites.

function Get-RunnerBasePin([string] $Dockerfile) {
    $lines = Get-Content -LiteralPath $Dockerfile
    $from = @($lines | Select-String -Pattern '^FROM\s')
    $pattern = '^FROM (?<image>ghcr\.io/actions/actions-runner:(?<version>(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)))@(?<digest>sha256:[0-9a-f]{64})\s*$'
    if ($from.Count -ne 1 -or $from[0].Line -notmatch $pattern) {
        throw "$Dockerfile must have exactly one FROM line, of the form FROM ghcr.io/actions/actions-runner:<version>@sha256:<digest>"
    }
    [pscustomobject]@{
        image = $Matches.image
        version = $Matches.version
        digest = $Matches.digest
        source = "https://github.com/actions/runner/releases/tag/v$($Matches.version)"
    }
}

# One pin per external Action repository, from every `uses: owner/repo[/path]@<sha> # vX.Y.Z`
# line outside the excluded repositories. Every use of a repository must agree.
function Get-WorkflowActionPins([string] $WorkflowDirectory, [string[]] $ExcludedRepositories = @()) {
    $pins = [ordered]@{}
    foreach ($file in Get-ChildItem -LiteralPath $WorkflowDirectory -Filter '*.yml' -File | Sort-Object Name) {
        $lineNumber = 0
        foreach ($line in Get-Content -LiteralPath $file.FullName) {
            $lineNumber++
            if ($line -notmatch '^\s*(?:-\s+)?uses:\s*(?<reference>[^\s#]+)(?<rest>.*)$') {
                continue
            }
            $reference = $Matches.reference
            $rest = $Matches.rest
            if ($reference.StartsWith('./')) {
                continue
            }
            $repository = ($reference -split '@', 2)[0] -replace '^([^/]+/[^/]+).*$', '$1'
            if ($ExcludedRepositories -contains $repository) {
                continue
            }
            $location = "$($file.Name):$lineNumber"
            if ($reference -notmatch '@(?<commit>[0-9a-f]{40})$' ) {
                throw "$location pins $reference, not a full commit SHA"
            }
            $commit = $Matches.commit
            if ($rest -notmatch '^\s+#\s+v(?<version>(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))\s*$') {
                throw "$location pins $repository without a same-line '# vX.Y.Z' release comment"
            }
            $version = $Matches.version
            if ($pins.Contains($repository)) {
                $first = $pins[$repository]
                if ($first.commit -ne $commit -or $first.version -ne $version) {
                    throw "$location pins $repository@$commit # v$version, but $($first.location) pins $($first.commit) # v$($first.version); every use must name one release"
                }
                continue
            }
            $pins[$repository] = [pscustomobject]@{
                repository = $repository
                version = $version
                commit = $commit
                location = $location
            }
        }
    }
    @($pins.Values)
}
