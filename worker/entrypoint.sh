#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$EUID" == "0" ]]; then
  echo 'worker must run as the non-root runner identity' >&2
  exit 77
fi

/usr/local/libexec/ci-runner-set-state idle

# JIT config arrives on stdin to stay out of `docker inspect`; the official
# runner masks it and removes ACTIONS_RUNNER_INPUT_* before job code runs.
if ! IFS= read -r jit_config || [[ -z "$jit_config" ]]; then
  echo 'worker JIT configuration was not provided' >&2
  exit 78
fi
export ACTIONS_RUNNER_INPUT_JITCONFIG="$jit_config"

if [[ "$#" == "0" ]]; then
  set -- /home/runner/run.sh
fi
exec "$@"
