# Immutable releases, digest-pinned images, and dependency freshness

Production installs consume a versioned Windows archive and an exact OCI digest
as one compatibility pair. Neither the release workflow nor provisioning uses a
mutable `latest` tag.

## Release output

A strict SemVer Git tag on a commit reachable from `main` runs the hosted
`Release` workflow. An active tag ruleset permits only organization owners to
create matching `v*` tags and blocks every other actor from creating, updating,
or deleting them. Build-metadata suffixes are rejected because OCI tag syntax
cannot preserve `+`. GitHub applies skip-check directives to tag-push events
before a workflow run exists. A commit selected for release must therefore not
contain `[skip ci]`, `[ci skip]`, `[no ci]`, `[skip actions]`, `[actions skip]`,
or a `skip-checks: true` trailer. Verify the exact target commit message before
creating its tag. If a protected tag is accidentally suppressed, the tag stays
reserved and immutable; publish the next patch version from a fresh reviewed
commit instead of moving or deleting the tag. `v0.1.6` is reserved by this rule
and has no release assets. A read-only job first reruns module verification, vet,
tests, race tests, vulnerability scanning, queue-monitor tests, Actionlint with
ShellCheck, Zizmor, Windows compilation, official-source dependency freshness,
and a local worker-image contract build against that exact tag. Only the
dependent publication job receives
`contents:write`, `packages:write`, `attestations:write`, and `id-token:write`.
It produces:

- `ci-runner-vX.Y.Z-windows-amd64.zip`, containing the interactive CLI, the
  windowless Task Scheduler controller, and the compatibility schema;
- an SPDX JSON SBOM for the Windows executables;
- `compatibility.json` with source SHA, archive digest, exact worker digest,
  runner/Scale Set Client/Go/PowerShell versions, the checksum-verified Buildx
  asset, exact BuildKit and SBOM-generator index/platform digests, and evidence
  references;
- `SHA256SUMS`;
- a Linux x64 worker image identified by its exact digest, with version and
  source-SHA tags promoted under the workflow's write-once contract;
- GitHub build-provenance attestations for every checksummed file and the OCI
  image; and BuildKit SBOM/provenance attestations attached to the image.

The image base, GitHub Actions, analyzers, and publication actions are immutable
pins. Same-line release comments allow Dependabot to recognize SHA-pinned
Actions. Every external Action also appears in `release/dependencies.json`; the
daily official-release check validates its current tag and tag-to-commit
mapping. Update PRs are review-only and are never auto-merged.

On a first run, the workflow pushes the BuildKit result without a tag, pulls and
contract-tests that exact registry digest, publishes and verifies the immutable
GitHub release, and only then promotes the same image index to both verified
version/source tags. Publication explicitly creates a source-marked draft, reconciles its
exact four assets by digest, and publishes it only after re-peeling the remote
tag to the event source SHA. A cancelled run can resume that exact owned draft;
ambiguous or foreign same-tag drafts fail closed. Lost responses after draft
creation, asset upload, and publication are covered by injected-failure tests.
A rerun accepts an existing published destination only after proving the release
is immutable, its asset set and checksums are exact, every asset and the worker
digest has the expected hosted-build attestation, the compatibility manifest
matches the reviewed dependency pins, and any existing OCI tag already resolves
to the proven digest. An absent destination may be created; an ambiguous lookup
or conflicting destination fails closed. All release workflows share one
non-cancelling maximal concurrency queue so this workflow's release requests
cannot race promotion. GHCR does not provide registry-enforced immutable tags:
another actor with package write access could still retarget either tag. The
workflow therefore treats each tag as write-once, rejects a conflicting
destination, verifies it after promotion, and deploys only by the exact digest.
Package write access must remain limited to the publication job. GitHub retains
at most 100 pending runs for `queue: max`; an
additional request is cancelled by the platform and must be deliberately
re-dispatched.

Provisioning first verifies the ZIP checksum from `SHA256SUMS`, then independently
runs `gh attestation verify --repo melodic-software/ci-runner` for the ZIP,
external `compatibility.json`, and `SHA256SUMS`. The release workflow attests all
three as separate subjects (and also attests the SBOM), so replacing the external
manifest and checksums cannot substitute a worker digest. Only after those checks
does provisioning read `compatibility.json` and change the `current` junction.
Rollback selects one of the retained prior manifests and digests; it never retags
an older image.

Signed release tags are optional future hardening, not a publication dependency.
The implemented trust chain is the protected source ref and exact source SHA,
GitHub-hosted OIDC build provenance, independently attested release subjects,
checksums, and the exact GHCR digest. Version/source tags are discoverability
aliases verified by the workflow, not deployment identities.

Authoritative references:

- [GitHub artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations)
- [GitHub immutable releases](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes)
- [GitHub release integrity verification](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/verify-release-integrity)
- [GitHub REST release and asset lifecycle](https://docs.github.com/en/rest/releases/releases)
- [GitHub CLI release creation and draft immutability boundary](https://cli.github.com/manual/gh_release_create)
- [GitHub workflow concurrency](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency)
- [GitHub Container registry digest pulls](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#pull-by-digest)
- [GitHub Packages permissions](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages)
- [GitHub immutable action guidance](https://docs.github.com/en/actions/reference/security/secure-use#using-third-party-actions)
- [Docker build attestations](https://docs.docker.com/build/metadata/attestations/)
- [Docker SBOM attestations](https://docs.docker.com/build/metadata/attestations/sbom/)
- [Anchore Syft releases](https://github.com/anchore/syft/releases)
- [Anchore Syft container](https://github.com/anchore/syft/pkgs/container/syft)
- [NIST Secure Software Development Framework](https://csrc.nist.gov/pubs/sp/800/218/final)

## Freshness policy

The scheduled drift check runs every day against official release APIs, the
Node.js LTS index, GHCR manifest, Go module proxy, analyzer release feeds, and GitHub's Ubuntu
24.04 hosted-image manifest. PowerShell tracks the hosted manifest rather than
getting ahead of it, preserving cloud parity. SBOM generation executes Anchore's
exact Syft image digest with no network, a read-only filesystem, no capabilities,
and a read-only artifact mount, so publication never downloads a mutable
installer or `latest`:

- create or refresh update evidence within 24 hours of detection;
- target validation and promotion within seven days;
- fail after 14 days of unresolved drift;
- fail immediately when runner release notes identify a critical/CVE release or
  when the digest behind the pinned runner tag changes;
- retain at least the latest three known-good compatibility pairs;
- never auto-merge controller, runner, image, toolchain, Scale Set Client,
  Action, or release changes.

GitHub progressively deploys runner versions. A release PR therefore must also
confirm the version offered in the organization runner setup UI/API before
promotion. A passing drift check is necessary but is not promotion approval.

Optional OTLP export remains a roadmap item. JSON Lines controller logs and the
compatibility manifest are structured so an exporter can be added without
changing lifecycle policy.
