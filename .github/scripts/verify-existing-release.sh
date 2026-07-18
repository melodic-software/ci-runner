#!/usr/bin/env bash
set -Eeuo pipefail

: "${GH_TOKEN:?GH_TOKEN is required}"
: "${RELEASE_VERSION:?RELEASE_VERSION is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_REF:?GITHUB_REF is required}"
: "${GITHUB_SHA:?GITHUB_SHA is required}"

mkdir -p dist
set +e
response="$(gh api --include "repos/$GITHUB_REPOSITORY/releases/tags/$RELEASE_VERSION" 2>&1)"
status=$?
set -e
if ((status != 0)); then
  status_line="${response%%$'\n'*}"
  if [[ "$status_line" =~ ^HTTP/[0-9.]+[[:space:]]+404([[:space:]]|$) ]]; then
    echo 'exists=false' >>"$GITHUB_OUTPUT"
    exit 0
  fi
  printf '%s\n' "$response" >&2
  echo 'Unable to distinguish an absent release from a GitHub API failure.' >&2
  exit 1
fi

node .github/scripts/release-transaction.cjs verify-tag
gh release verify "$RELEASE_VERSION" --repo "$GITHUB_REPOSITORY"
archive="ci-runner-${RELEASE_VERSION}-windows-amd64.zip"
sbom="ci-runner-${RELEASE_VERSION}-windows-amd64.spdx.json"
expected=(SHA256SUMS "$archive" "$sbom" compatibility.json)
mapfile -t expected < <(printf '%s\n' "${expected[@]}" | LC_ALL=C sort)
mapfile -t release_assets < <(
  gh release view "$RELEASE_VERSION" --repo "$GITHUB_REPOSITORY" --json assets \
    --jq '.assets[].name' | LC_ALL=C sort
)
[[ "${release_assets[*]}" == "${expected[*]}" ]]
for asset in "$archive" "$sbom" compatibility.json SHA256SUMS; do
  gh release download "$RELEASE_VERSION" --repo "$GITHUB_REPOSITORY" --dir dist --pattern "$asset"
  gh release verify-asset "$RELEASE_VERSION" "dist/$asset" --repo "$GITHUB_REPOSITORY"
done
mapfile -t assets < <(find dist -maxdepth 1 -type f -printf '%f\n' | LC_ALL=C sort)
[[ "${assets[*]}" == "${expected[*]}" ]]
mapfile -t checksum_names < <(sed --regexp-extended --silent 's/^[0-9a-f]{64}  ([^/\\]+)$/\1/p' dist/SHA256SUMS | LC_ALL=C sort)
expected_checksum_names=("$archive" "$sbom" compatibility.json)
mapfile -t expected_checksum_names < <(printf '%s\n' "${expected_checksum_names[@]}" | LC_ALL=C sort)
[[ "${checksum_names[*]}" == "${expected_checksum_names[*]}" ]]
(cd dist && sha256sum --check --strict SHA256SUMS)

manifest=dist/compatibility.json
archive_digest="sha256:$(sha256sum "dist/$archive" | cut --delimiter=' ' --fields=1)"
jq --exit-status \
  --arg version "$RELEASE_VERSION" \
  --arg repository "$GITHUB_REPOSITORY" \
  --arg sha "$GITHUB_SHA" \
  --arg archive "$archive" \
  --arg archive_digest "$archive_digest" \
  --arg sbom "$sbom" \
  --slurpfile pins release/dependencies.json \
  '
    (keys | sort) == (["controller","createdAt","dependencies","evidence","releaseVersion","schemaVersion","source","worker"] | sort)
    and .schemaVersion == 1
    and .releaseVersion == $version
    and .source.repository == $repository
    and .source.sha == $sha
    and .controller.version == ($version | ltrimstr("v"))
    and .controller.windowsArchive == $archive
    and .controller.archiveDigest == $archive_digest
    and .worker.image == ("ghcr.io/" + $repository)
    and (.worker.digest | test("^sha256:[0-9a-f]{64}$"))
    and .dependencies == {
      runnerVersion: $pins[0].runner.version,
      scaleSetClientVersion: $pins[0].scaleSetClient.version,
      scaleSetClientCommit: $pins[0].scaleSetClient.commit,
      goToolchain: ("go" + $pins[0].go.version),
      powerShellVersion: $pins[0].powerShell.version,
      ghVersion: $pins[0].gh.version,
      ghLinuxAmd64ArchiveSha256: $pins[0].gh.linuxAmd64ArchiveSha256,
      buildxVersion: $pins[0].buildx.version,
      buildxLinuxAmd64Sha256: $pins[0].buildx.linuxAmd64Sha256,
      buildKitVersion: $pins[0].buildKit.version,
      buildKitDigest: $pins[0].buildKit.digest,
      buildKitLinuxAmd64Digest: $pins[0].buildKit.linuxAmd64Digest,
      sbomGeneratorVersion: $pins[0].buildKitSbomScanner.version,
      sbomGeneratorDigest: $pins[0].buildKitSbomScanner.digest,
      sbomGeneratorLinuxAmd64Digest: $pins[0].buildKitSbomScanner.linuxAmd64Digest
    }
    and .evidence == {
      checksums: "SHA256SUMS",
      controllerSbom: $sbom,
      controllerProvenance: ("https://github.com/" + $repository + "/attestations"),
      workerSbom: ("oci://" + .worker.image + "@" + .worker.digest + "#sbom"),
      workerProvenance: ("oci://" + .worker.image + "@" + .worker.digest + "#provenance")
    }
  ' "$manifest" >/dev/null

version="${RELEASE_VERSION#v}"
version_symbol='github.com/melodic-software/ci-runner/internal/buildinfo.Version'
go run -trimpath -ldflags="-X ${version_symbol}=${version}" ./cmd/ci-runner \
  release validate --manifest "$(realpath "$manifest")" --version "$version" >/dev/null

signer="$GITHUB_REPOSITORY/.github/workflows/release.yml"
for asset in "${expected[@]}"; do
  gh attestation verify "dist/$asset" \
    --repo "$GITHUB_REPOSITORY" \
    --signer-workflow "$signer" \
    --source-ref "$GITHUB_REF" \
    --source-digest "$GITHUB_SHA" \
    --deny-self-hosted-runners
done
worker_digest="$(jq --exit-status --raw-output '.worker.digest' "$manifest")"
gh attestation verify "oci://ghcr.io/$GITHUB_REPOSITORY@$worker_digest" \
  --repo "$GITHUB_REPOSITORY" \
  --signer-workflow "$signer" \
  --source-ref "$GITHUB_REF" \
  --source-digest "$GITHUB_SHA" \
  --deny-self-hosted-runners
node .github/scripts/release-transaction.cjs verify-tag
echo 'exists=true' >>"$GITHUB_OUTPUT"
echo "worker_digest=$worker_digest" >>"$GITHUB_OUTPUT"
