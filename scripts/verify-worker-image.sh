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
  for command in sudo pwsh gh git git-lfs curl jq zip unzip ssh clang; do
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
    /usr/local/libexec/ci-runner-capture-cgroup \
    /usr/local/libexec/ci-runner-job-started.sh \
    /usr/local/libexec/ci-runner-job-completed.sh; do
    test "$(stat --format="%U:%G:%a" "$hook")" = "root:root:555"
  done
'

run_with_runner_output_captured() {
  local script="$1" sidecar_destination="$2" container_id exit_code logs
  container_id="$(docker create --interactive --log-driver local "$image" /bin/bash -Eeuo pipefail -c "$script")"
  if [[ "$(docker inspect --format '{{.HostConfig.LogConfig.Type}}' "$container_id")" != local ]]; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  if ! printf 'test-jit\n' | docker start --attach --interactive "$container_id" >/dev/null; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  exit_code="$(docker inspect --format '{{.State.ExitCode}}' "$container_id")"
  if [[ "$exit_code" != 0 ]]; then
    docker logs "$container_id" >&2 || true
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  if ! logs="$(docker logs "$container_id")"; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  if ! docker cp "$container_id":/home/runner/_runner_state/cgroup-terminal.json - |
    tar --extract --to-stdout >"$sidecar_destination"; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  docker rm "$container_id" >/dev/null
  printf '%s' "$logs"
}

temporary_sidecars=()
cleanup_sidecars() {
  if ((${#temporary_sidecars[@]} > 0)); then
    rm --force "${temporary_sidecars[@]}"
  fi
}
trap cleanup_sidecars EXIT

# ScriptHandler redirects hook stdout/stderr and OutputManager consumes those
# streams. Run the hook as a redirected child of the same-UID PID 1, then read
# the stopped container through `docker logs`. A marker here proves it bypassed
# the runner capture boundary through PID 1's Docker logging pipe.
hook_script=
read -r -d '' hook_script <<'CONTAINER_SCRIPT' || true
  initial="$(cat /home/runner/_runner_state/state)"
  /usr/local/libexec/ci-runner-job-started.sh
  busy="$(cat /home/runner/_runner_state/state)"
  runner_output="$(mktemp)"
  /bin/bash -Eeuo pipefail -c "printf \"runner-output-sentinel\\n\"; /usr/local/libexec/ci-runner-job-completed.sh" >"$runner_output" 2>&1
  completed="$(cat /home/runner/_runner_state/state)"
  test "$(cat "$runner_output")" = runner-output-sentinel
  test "$initial" = idle
  test "$busy" = busy
  test "$completed" = completed
  test "$(stat --format=%u /proc/1)" = "$(id -u)"
  [[ "$(readlink /proc/1/fd/1)" == pipe:\[*\] ]]
  test "$(stat --format="%U:%G:%a" /home/runner/_runner_state/cgroup-terminal.json)" = "runner:runner:600"
  test "$(wc --lines </home/runner/_runner_state/cgroup-terminal.json)" = 1
  ! compgen -G "/home/runner/_runner_state/.cgroup-terminal.*" >/dev/null
  capture_line="$(grep --line-number --fixed-strings "/usr/local/libexec/ci-runner-capture-cgroup" /usr/local/libexec/ci-runner-job-completed.sh | cut --delimiter=: --fields=1)"
  completed_line="$(grep --line-number --fixed-strings "/usr/local/libexec/ci-runner-set-state completed" /usr/local/libexec/ci-runner-job-completed.sh | cut --delimiter=: --fields=1)"
  test "$capture_line" -lt "$completed_line"
CONTAINER_SCRIPT
hook_sidecar="$(mktemp)"
temporary_sidecars+=("$hook_sidecar")
hook_logs="$(run_with_runner_output_captured "$hook_script" "$hook_sidecar")"
readonly resource_marker_prefix=ci-runner-resource-evidence-v1:
mapfile -t hook_lines <<<"$hook_logs"
[[ "${#hook_lines[@]}" == 1 ]]
[[ "$hook_logs" != *runner-output-sentinel* ]]
resource_marker="${hook_lines[0]}"
resource_evidence="$(<"$hook_sidecar")"
[[ "$resource_marker" == "$resource_marker_prefix$resource_evidence" ]]
[[ "${#resource_evidence}" -le 32768 ]]
[[ "${#resource_marker}" -lt 4096 ]]
[[ "$(jq --compact-output . <<<"$resource_evidence")" == "$resource_evidence" ]]
jq --exit-status '
  .schemaVersion == 1 and
  .source == "cgroup-v2" and
  (.status == "complete" or .status == "partial" or .status == "unavailable") and
  (.missing | type == "array") and
  (.memory.peakBytes | type == "number") and
  (.memory.swapPeakBytes | type == "number") and
  (.memory.oomEvents | type == "number") and
  (.memory.oomKillEvents | type == "number") and
  (.cpu.periods | type == "number") and
  (.cpu.throttledPeriods | type == "number") and
  (.cpu.throttledMicroseconds | type == "number") and
  (.pids.peak | type == "number") and
  (.io.readBytes | type == "number") and
  (.io.writeBytes | type == "number")
' <<<"$resource_evidence" >/dev/null

# Exercise the capture script against a controlled cgroup-v2 fixture. This
# catches awk portability failures and proves empty/malformed io.stat cannot be
# reported as a measured zero.
fixture_script=
read -r -d '' fixture_script <<'CONTAINER_SCRIPT' || true
  fixture="$(mktemp --directory)"
  trap 'rm --force --recursive "$fixture"' EXIT
  printf "1024\n" >"$fixture/memory.peak"
  printf "0\n" >"$fixture/memory.swap.peak"
  printf "oom 0\noom_kill 0\n" >"$fixture/memory.events"
  printf "nr_periods 10\nnr_throttled 2\nthrottled_usec 50\n" >"$fixture/cpu.stat"
  printf "4\n" >"$fixture/pids.peak"
  for io_content in "" "8:0 rbytes=broken wbytes=20"; do
    printf "%s" "$io_content" >"$fixture/io.stat"
    runner_output="$(mktemp)"
    /usr/local/libexec/ci-runner-capture-cgroup "$fixture" >"$runner_output" 2>&1
    test ! -s "$runner_output"
    test "$(stat --format="%U:%G:%a" /home/runner/_runner_state/cgroup-terminal.json)" = "runner:runner:600"
  done
CONTAINER_SCRIPT
fixture_sidecar="$(mktemp)"
temporary_sidecars+=("$fixture_sidecar")
fixture_markers="$(run_with_runner_output_captured "$fixture_script" "$fixture_sidecar")"
mapfile -t fixture_lines <<<"$fixture_markers"
[[ "${#fixture_lines[@]}" == 2 ]]
for fixture_marker in "${fixture_lines[@]}"; do
  [[ "$fixture_marker" == "$resource_marker_prefix"* ]]
  fixture_record="${fixture_marker#"$resource_marker_prefix"}"
  [[ "${#fixture_record}" -le 32768 ]]
  [[ "${#fixture_marker}" -lt 4096 ]]
  jq --exit-status '
    .status == "partial" and
    (.missing | index("io.stat") != null) and
    .io.readBytes == 0 and
    .io.writeBytes == 0
  ' <<<"$fixture_record" >/dev/null
done
fixture_evidence="$(<"$fixture_sidecar")"
[[ "${fixture_lines[1]}" == "$resource_marker_prefix$fixture_evidence" ]]

unavailable_script=
read -r -d '' unavailable_script <<'CONTAINER_SCRIPT' || true
  fixture="$(mktemp --directory)"
  trap 'rm --force --recursive "$fixture"' EXIT
  runner_output="$(mktemp)"
  /usr/local/libexec/ci-runner-capture-cgroup "$fixture" >"$runner_output" 2>&1
  test ! -s "$runner_output"
CONTAINER_SCRIPT
unavailable_sidecar="$(mktemp)"
temporary_sidecars+=("$unavailable_sidecar")
unavailable_logs="$(run_with_runner_output_captured "$unavailable_script" "$unavailable_sidecar")"
mapfile -t unavailable_lines <<<"$unavailable_logs"
[[ "${#unavailable_lines[@]}" == 1 ]]
unavailable_marker="${unavailable_lines[0]}"
unavailable_record="$(<"$unavailable_sidecar")"
[[ "$unavailable_marker" == "$resource_marker_prefix$unavailable_record" ]]
[[ "${#unavailable_record}" -le 32768 ]]
[[ "${#unavailable_marker}" -lt 4096 ]]
jq --exit-status '
  .status == "unavailable" and
  (.missing | length) == 9 and
  ([.memory[], .cpu[], .pids[], .io[]] | all(. == 0))
' <<<"$unavailable_record" >/dev/null

if docker run --rm "$image" /bin/true; then
  echo 'worker accepted a start without controller-provided JIT input' >&2
  exit 1
fi

echo "verified immutable one-job worker image: $image"
