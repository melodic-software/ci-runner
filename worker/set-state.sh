#!/usr/bin/env bash
set -Eeuo pipefail

readonly state="${1:?state is required}"
case "$state" in
idle | busy | completed) ;;
*)
  echo "invalid worker state: $state" >&2
  exit 64
  ;;
esac

readonly state_directory=/home/runner/_runner_state
readonly state_file="$state_directory/state"

umask 077
mkdir --parents "$state_directory"
temporary="$(mktemp "$state_directory/.state.XXXXXX")"
trap 'rm --force "$temporary"' EXIT
printf '%s' "$state" >"$temporary"
chmod 0600 "$temporary"
mv --force "$temporary" "$state_file"
trap - EXIT
