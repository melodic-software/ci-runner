#!/usr/bin/env bash
set -Eeuo pipefail

image="${1:?usage: verify-worker-image.sh IMAGE}"
dependencies="${2:-release/dependencies.json}"

expected_runner_version="$(jq --exit-status --raw-output '.runner.version' "$dependencies")"
expected_base_digest="$(jq --exit-status --raw-output '.runner.digest' "$dependencies")"

config="$(docker image inspect "$image" --format '{{json .Config}}')"

image_env() {
  local name="$1"
  jq --exit-status --raw-output --arg name "$name" '
    [.Env[] | select(startswith($name + "="))]
    | if length == 1 then .[0] else error("expected exactly one " + $name + " image variable") end
  ' <<<"$config"
}

[[ "$(jq --raw-output '.User' <<<"$config")" == "runner" ]]
[[ "$(jq --raw-output '.WorkingDir' <<<"$config")" == "/home/runner" ]]
[[ "$(jq --compact-output '.Cmd' <<<"$config")" == '["/home/runner/run.sh"]' ]]
[[ "$(jq --compact-output '.Entrypoint' <<<"$config")" == '["/usr/local/bin/ci-runner-entrypoint"]' ]]
[[ "$(jq --compact-output '.Volumes' <<<"$config")" == 'null' ]]
[[ "$(jq --raw-output '.Labels["org.opencontainers.image.base.digest"]' <<<"$config")" == "$expected_base_digest" ]]
[[ "$(jq --raw-output '.Env[] | select(startswith("ACTIONS_RUNNER_HOOK_JOB_STARTED="))' <<<"$config")" == 'ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/libexec/ci-runner-job-started.sh' ]]
[[ "$(jq --raw-output '.Env[] | select(startswith("ACTIONS_RUNNER_HOOK_JOB_COMPLETED="))' <<<"$config")" == 'ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/usr/local/libexec/ci-runner-job-completed.sh' ]]
[[ "$(image_env DOTNET_INSTALL_DIR)" == 'DOTNET_INSTALL_DIR=/home/runner/.dotnet' ]]
[[ "$(image_env DOTNET_ROOT)" == 'DOTNET_ROOT=/home/runner/.dotnet' ]]
[[ "$(image_env NUGET_PACKAGES)" == 'NUGET_PACKAGES=/home/runner/.nuget/packages' ]]
[[ "$(image_env PATH)" == 'PATH=/home/runner/.dotnet:/home/runner/.dotnet/tools:'* ]]

for dynamic_runner_variable in RUNNER_TOOL_CACHE RUNNER_TOOLSDIRECTORY AGENT_TOOLSDIRECTORY; do
  if jq --exit-status --arg name "$dynamic_runner_variable" \
    '.Env[] | select(startswith($name + "="))' <<<"$config" >/dev/null; then
    echo "worker image must not override runner-managed $dynamic_runner_variable" >&2
    exit 1
  fi
done

if jq --exit-status '.Env[] | select(test("(APP_PRIVATE|PRIVATE_KEY|INSTALLATION_TOKEN|JITCONFIG|GITHUB_TOKEN)"; "i"))' <<<"$config" >/dev/null; then
  echo "worker image config contains a forbidden credential/JIT environment variable" >&2
  exit 1
fi

actual_runner_version="$({
  docker run --rm --entrypoint /home/runner/bin/Runner.Listener "$image" --version
} | tr -d '\r' | grep --extended-regexp '^[0-9]+\.[0-9]+\.[0-9]+$' | tail --lines=1)"
[[ "$actual_runner_version" == "$expected_runner_version" ]]

docker run --rm --entrypoint /bin/bash "$image" -Eeuo pipefail -c '
  [[ "$(id -u)" == "1001" ]]
  [[ "$(id -g)" == "1001" ]]
  [[ "$(. /etc/os-release && printf "%s" "$VERSION_ID")" == "24.04" ]]
  for command in sudo pwsh git git-lfs curl jq zip unzip ssh clang; do
    command -v "$command" >/dev/null
  done
  sudo -n true
  test -f /usr/include/zlib.h
  test -z "${ACTIONS_RUNNER_INPUT_JITCONFIG:-}"
  test "${ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT:-}" = "1"
  test "${DOTNET_INSTALL_DIR:-}" = "/home/runner/.dotnet"
  test "${DOTNET_ROOT:-}" = "$DOTNET_INSTALL_DIR"
  test "${NUGET_PACKAGES:-}" = "/home/runner/.nuget/packages"
  [[ "$PATH" == "$DOTNET_INSTALL_DIR:$DOTNET_INSTALL_DIR/tools:"* ]]
  test -z "${RUNNER_TOOL_CACHE:-}"
  test -z "${RUNNER_TOOLSDIRECTORY:-}"
  test -z "${AGENT_TOOLSDIRECTORY:-}"
  if command -v dotnet >/dev/null; then
    echo ".NET SDK/runtime must come from the workflow setup action" >&2
    exit 1
  fi
  for directory in \
    "$DOTNET_INSTALL_DIR" \
    "$DOTNET_INSTALL_DIR/tools" \
    /home/runner/.nuget \
    "$NUGET_PACKAGES"; do
    test "$(stat --format="%U:%G:%a" "$directory")" = "runner:runner:755"
    test -w "$directory"
    touch "$directory/.ci-runner-write-test"
    rm --force "$directory/.ci-runner-write-test"
  done
  for hook in \
    /usr/local/bin/ci-runner-entrypoint \
    /usr/local/libexec/ci-runner-set-state \
    /usr/local/libexec/ci-runner-job-started.sh \
    /usr/local/libexec/ci-runner-job-completed.sh; do
    test "$(stat --format="%U:%G:%a" "$hook")" = "root:root:555"
  done
'

states="$(printf 'test-jit\n' | docker run --rm --interactive "$image" /bin/bash -Eeuo pipefail -c '
  cat /home/runner/_runner_state/state
  printf " "
  /usr/local/libexec/ci-runner-job-started.sh
  cat /home/runner/_runner_state/state
  printf " "
  /usr/local/libexec/ci-runner-job-completed.sh
  cat /home/runner/_runner_state/state
')"
[[ "$states" == 'idle busy completed' ]]

if docker run --rm "$image" /bin/true; then
  echo 'worker accepted a start without controller-provided JIT input' >&2
  exit 1
fi

echo "verified immutable one-job worker image: $image"
