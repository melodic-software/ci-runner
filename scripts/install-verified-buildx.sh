#!/usr/bin/env bash
set -Eeuo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dependencies="$repository_root/release/dependencies.json"
version="$(jq --exit-status --raw-output '.buildx.version' "$dependencies")"
expected_sha256="$(jq --exit-status --raw-output '.buildx.linuxAmd64Sha256' "$dependencies")"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]]

asset="buildx-v${version}.linux-amd64"
url="https://github.com/docker/buildx/releases/download/v${version}/${asset}"
temporary_directory="$(mktemp --directory)"
trap 'rm -rf -- "$temporary_directory"' EXIT

curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --retry-all-errors \
  --output "$temporary_directory/$asset" "$url"
printf '%s  %s\n' "$expected_sha256" "$temporary_directory/$asset" | sha256sum --check --strict -

plugin_directory="$HOME/.docker/cli-plugins"
mkdir --parents -- "$plugin_directory"
install --mode 0755 -- "$temporary_directory/$asset" "$plugin_directory/docker-buildx"

observed="$(docker buildx version)"
if [[ "$observed" != "github.com/docker/buildx v${version} "* ]]; then
  echo "Verified Buildx plugin reported an unexpected version: $observed" >&2
  exit 1
fi
