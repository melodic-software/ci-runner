#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$(id -u)" == "0" ]]; then
  echo 'worker must run as the non-root runner identity' >&2
  exit 77
fi

/usr/local/libexec/ci-runner-set-state idle

# The controller sends the one-job JIT configuration over attached stdin after
# container creation. This keeps the secret out of Docker's persistent
# container configuration and `docker inspect`; the official runner then masks
# the value and removes ACTIONS_RUNNER_INPUT_* before it launches job code.
if ! IFS= read -r jit_config; then
  echo 'worker JIT configuration was not provided' >&2
  exit 78
fi
if [[ -z "$jit_config" ]]; then
  echo 'worker JIT configuration was not provided' >&2
  exit 78
fi
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit_config"

if [[ "$#" == "0" ]]; then
  set -- /home/runner/run.sh
fi
exec "$@"
