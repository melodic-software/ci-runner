#!/bin/bash
# Registers this container as an ephemeral just-in-time (JIT) runner and runs
# exactly one job: GitHub App JWT -> installation access token (scoped to
# organization self-hosted runners) -> generate-jitconfig -> run.sh. No
# long-lived registration token ever exists; the App private key (mounted
# read-only) is the only durable credential, and it never leaves this step.
set -Eeuo pipefail

# A failed registration exits the container and `restart: always` retries it;
# pause first so a persistent failure (bad key, revoked App) cannot hammer the
# GitHub API in a tight restart loop.
trap 'echo "entrypoint failed; pausing before container restart" >&2; sleep 15' ERR

: "${APP_CLIENT_ID:?GitHub App client ID is required}"
KEY_FILE="${APP_PRIVATE_KEY_FILE:-/run/secrets/github-app-key.pem}"
ORG="${ORG:-melodic-software}"
RUNNER_GROUP="${RUNNER_GROUP:-self-hosted}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted-medley}"
# Compose gives every replica a unique hostname; the timestamp disambiguates
# successive registrations from the same container across restarts.
RUNNER_NAME="${RUNNER_NAME_PREFIX:-runner}-$(hostname)-$(date +%s)"

[[ -r "$KEY_FILE" ]] || { echo "App private key not readable at $KEY_FILE" >&2; exit 1; }

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# App JWT (RS256, client ID as issuer); 5-minute lifetime is ample for the
# three calls below, and iat is backdated 60s to absorb clock skew.
now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 300))" "$APP_CLIENT_ID" | b64url)
signature=$(printf '%s.%s' "$header" "$payload" | openssl dgst -sha256 -sign "$KEY_FILE" -binary | b64url)
jwt="$header.$payload.$signature"

api() {
  local auth="$1"
  shift
  curl -fsS \
    -H "Authorization: Bearer $auth" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$@"
}

installation_id=$(api "$jwt" "https://api.github.com/orgs/$ORG/installation" | jq -er '.id')

# Downscope the installation token to the one permission this flow needs, even
# if the App itself ever grows more.
token=$(api "$jwt" -X POST "https://api.github.com/app/installations/$installation_id/access_tokens" \
  -d '{"permissions":{"organization_self_hosted_runners":"write"}}' | jq -er '.token')

group_id=$(api "$token" "https://api.github.com/orgs/$ORG/actions/runner-groups?per_page=100" \
  | jq -er --arg name "$RUNNER_GROUP" '.runner_groups[] | select(.name == $name) | .id') \
  || { echo "runner group '$RUNNER_GROUP' not found in org $ORG" >&2; exit 1; }

jit_request=$(jq -cn \
  --arg name "$RUNNER_NAME" \
  --argjson group_id "$group_id" \
  --arg labels "$RUNNER_LABELS" \
  '{name: $name, runner_group_id: $group_id, labels: ($labels | split(","))}')

encoded_jit_config=$(api "$token" -X POST "https://api.github.com/orgs/$ORG/actions/runners/generate-jitconfig" \
  -d "$jit_request" | jq -er '.encoded_jit_config')

unset token jwt signature payload

echo "registered JIT runner '$RUNNER_NAME' (group '$RUNNER_GROUP', labels '$RUNNER_LABELS'); waiting for one job"
exec ./run.sh --jitconfig "$encoded_jit_config"
