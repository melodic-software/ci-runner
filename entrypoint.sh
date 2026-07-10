#!/bin/bash
# Registers this container as an ephemeral just-in-time (JIT) runner and runs
# exactly one job: GitHub App JWT -> installation access token (scoped to
# organization self-hosted runners) -> generate-jitconfig -> run.sh. No
# long-lived registration token ever exists, and the App private key is never
# written to a filesystem: it arrives base64-encoded in the environment, is
# consumed via process substitution for the one JWT signature, and PID 1's
# original environment is readable only by root while the job itself runs
# de-privileged as 'runner' (see the setpriv exec at the bottom).
set -Eeuo pipefail

# Rate-limit ALL failure exits: `restart: always` retries the container, and a
# persistent failure (bad key, revoked App, wrong group) must not hammer the
# GitHub API in a tight loop. fail() covers handled errors (Bash skips the ERR
# trap for commands tested by ||); the trap covers everything unexpected.
fail() {
  echo "$*" >&2
  sleep 15
  exit 1
}
# errexit already exits after this trap (verified empirically); the explicit
# exit is belt-and-braces for any errexit-suppressed context.
trap 'echo "entrypoint failed; pausing before container restart" >&2; sleep 15; exit 1' ERR

: "${APP_CLIENT_ID:?GitHub App client ID is required}"
: "${APP_PRIVATE_KEY_B64:?base64-encoded GitHub App private key is required}"
ORG="${ORG:-melodic-software}"
RUNNER_GROUP="${RUNNER_GROUP:-self-hosted}"
RUNNER_LABELS="${RUNNER_LABELS:-self-hosted-medley}"
# Compose gives every replica a unique hostname; the timestamp disambiguates
# successive registrations from the same container across restarts.
RUNNER_NAME="${RUNNER_NAME_PREFIX:-runner}-$(hostname)-$(date +%s)"

[[ "$(id -u)" -eq 0 ]] || fail "entrypoint must start as root; it drops to 'runner' before the job runs"

# Docker's restart policy restarts the SAME container, so a finished job's
# writable layer survives into the next registration. Scrub the job-visible
# state (work dir, temp — find -delete, since globs miss dotfiles) before
# taking a new job. /opt/hostedtoolcache is deliberately kept: re-downloading
# toolchains every job costs more than the poisoning risk on a fleet that only
# ever runs this org's private, first-party code; image updates (compose pull)
# still recreate containers outright.
rm -rf ./_work 2>/dev/null || true
find /tmp /var/tmp -mindepth 1 -delete 2>/dev/null || true

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

# App JWT (RS256, client ID as issuer); 5-minute lifetime is ample for the
# three calls below, and iat is backdated 60s to absorb clock skew. The key is
# piped straight from the environment into the signature — never a file.
now=$(date +%s)
header=$(printf '{"alg":"RS256","typ":"JWT"}' | b64url)
payload=$(printf '{"iat":%d,"exp":%d,"iss":"%s"}' "$((now - 60))" "$((now + 300))" "$APP_CLIENT_ID" | b64url)
signature=$(printf '%s.%s' "$header" "$payload" |
  openssl dgst -sha256 -sign <(printf '%s' "$APP_PRIVATE_KEY_B64" | base64 -d) -binary | b64url)
jwt="$header.$payload.$signature"
# Drop the key from the live environment. PID 1's ORIGINAL environ remains
# visible at /proc/1/environ to root only - acceptable because the job runs as
# 'runner' (root escalation implies sudo, see the README threat model).
unset APP_PRIVATE_KEY_B64

api() {
  local auth="$1"
  shift
  curl -fsS \
    -H "Authorization: Bearer $auth" \
    -H "Accept: application/vnd.github+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    -H "Content-Type: application/json" \
    "$@"
}

installation_response=$(api "$jwt" "https://api.github.com/orgs/$ORG/installation")
installation_id=$(jq -er '.id' <<<"$installation_response") ||
  fail "could not resolve the App installation for org $ORG"

# Downscope the installation token to the one permission this flow needs, even
# if the App itself ever grows more.
token_response=$(api "$jwt" -X POST "https://api.github.com/app/installations/$installation_id/access_tokens" \
  -d '{"permissions":{"organization_self_hosted_runners":"write"}}')
token=$(jq -er '.token' <<<"$token_response") ||
  fail "could not mint a downscoped installation token"

# Single page: the org has a handful of runner groups (2 at the time of
# writing); revisit with pagination only if that ever approaches 100.
groups_response=$(api "$token" "https://api.github.com/orgs/$ORG/actions/runner-groups?per_page=100")
group_id=$(jq -er --arg name "$RUNNER_GROUP" \
  '.runner_groups[] | select(.name == $name) | .id' <<<"$groups_response") ||
  fail "runner group '$RUNNER_GROUP' not found in org $ORG"

# Labels: comma-separated, whitespace-trimmed, empties dropped - so
# "a, b," registers labels 'a' and 'b', not ' b' (which runs-on would miss).
jit_request=$(jq -cn \
  --arg name "$RUNNER_NAME" \
  --argjson group_id "$group_id" \
  --arg labels "$RUNNER_LABELS" \
  '{name: $name, runner_group_id: $group_id,
    labels: ($labels | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0)))}')

jit_response=$(api "$token" -X POST "https://api.github.com/orgs/$ORG/actions/runners/generate-jitconfig" \
  -d "$jit_request")
encoded_jit_config=$(jq -er '.encoded_jit_config' <<<"$jit_response") ||
  fail "generate-jitconfig failed for runner $RUNNER_NAME"

unset token token_response jwt signature payload \
  installation_response groups_response jit_response

echo "registered JIT runner '$RUNNER_NAME' (group '$RUNNER_GROUP', labels '$RUNNER_LABELS'); waiting for one job"
# Drop root for good: the runner - and every job step it spawns - runs as the
# unprivileged 'runner' user. setpriv execs with no intermediate process, so
# signals from docker stop reach the runner directly.
exec setpriv --reuid runner --regid runner --init-groups \
  env HOME=/home/runner USER=runner LOGNAME=runner \
  ./run.sh --jitconfig "$encoded_jit_config"
