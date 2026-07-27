#!/usr/bin/env bash
set -Eeuo pipefail
# Every transport helper runs inside a command substitution. Without this, bash
# clears errexit in that subshell and a failed contract assertion is swallowed:
# the helper runs on to its final printf and the verifier reports success.
shopt -s inherit_errexit

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
[[ "$(image_env RUNNER_MANUALLY_TRAP_SIG)" == 'RUNNER_MANUALLY_TRAP_SIG=1' ]]

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

temporary_sidecars=()
temporary_containers=()
temporary_images=()
cleanup_verifier_resources() {
  local container verifier_image
  for container in "${temporary_containers[@]}"; do
    docker rm --force "$container" >/dev/null 2>&1 || true
  done
  for verifier_image in "${temporary_images[@]}"; do
    docker image rm --force "$verifier_image" >/dev/null 2>&1 || true
  done
  if ((${#temporary_sidecars[@]} > 0)); then
    rm --force "${temporary_sidecars[@]}"
  fi
}
trap cleanup_verifier_resources EXIT

run_fixture_with_captured_output() {
  local script="$1" sidecar_destination="$2" container_id exit_code logs
  container_id="$(docker create --interactive --log-driver local "$image" /bin/bash -Eeuo pipefail -c "$script")"
  temporary_containers+=("$container_id")
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

production_harness_image="ci-runner-worker-production-cmd-verifier:${BASHPID}-${RANDOM}"
temporary_images+=("$production_harness_image")
docker build --quiet \
  --build-arg "BASE_IMAGE=$image" \
  --tag "$production_harness_image" \
  scripts/fixtures/production-cmd-harness >/dev/null
harness_config="$(docker image inspect "$production_harness_image" --format '{{json .Config}}')"
[[ "$harness_config" == "$config" ]]

run_production_command_transport() {
  local container_id exit_code logs
  container_id="$(docker create --interactive --log-driver local "$production_harness_image")"
  temporary_containers+=("$container_id")
  [[ "$(docker inspect --format '{{.Path}}' "$container_id")" == /usr/local/bin/ci-runner-entrypoint ]]
  [[ "$(docker inspect --format '{{json .Args}}' "$container_id")" == '["/home/runner/run.sh"]' ]]
  [[ "$(docker inspect --format '{{.Config.User}}' "$container_id")" == runner ]]
  [[ "$(docker inspect --format '{{.HostConfig.LogConfig.Type}}' "$container_id")" == local ]]
  [[ "$(docker inspect --format '{{.HostConfig.Privileged}}' "$container_id")" == false ]]
  [[ "$(docker inspect --format '{{json .HostConfig.Binds}}' "$container_id")" == null ]]
  [[ "$(docker inspect --format '{{json .HostConfig.CapAdd}}' "$container_id")" == null ]]
  [[ "$(docker inspect --format '{{json .Mounts}}' "$container_id")" == '[]' ]]
  if ! printf 'test-jit\n' | timeout 60s docker start --attach --interactive "$container_id" >/dev/null; then
    docker logs --timestamps "$container_id" >&2 || true
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  exit_code="$(docker inspect --format '{{.State.ExitCode}}' "$container_id")"
  if [[ "$exit_code" != 0 ]]; then
    docker logs --timestamps "$container_id" >&2 || true
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  if ! logs="$(docker logs --timestamps "$container_id" 2>&1)"; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
    return 1
  fi
  docker rm "$container_id" >/dev/null
  printf '%s' "$logs"
}

readonly resource_marker_prefix=ci-runner-resource-evidence-v1:
readonly harness_sentinel=ci-runner-production-cmd-verifier-v1
readonly sidecar_digest_prefix=ci-runner-sidecar-sha256:
readonly pipe_buffer_prefix=ci-runner-stdout-pipe-buf:
production_logs="$(run_production_command_transport)"
mapfile -t production_lines <<<"$production_logs"
resource_marker=
sidecar_digest=
pipe_buffer=
marker_count=0
sentinel_count=0
digest_count=0
pipe_buffer_count=0
for timestamped_line in "${production_lines[@]}"; do
  timestamp="${timestamped_line%% *}"
  line="${timestamped_line#* }"
  [[ "$timestamp" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$ ]]
  case "$line" in
  "$resource_marker_prefix"*)
    marker_count=$((marker_count + 1))
    resource_marker="$line"
    ;;
  "$harness_sentinel")
    sentinel_count=$((sentinel_count + 1))
    ;;
  "$sidecar_digest_prefix"*)
    digest_count=$((digest_count + 1))
    sidecar_digest="${line#"$sidecar_digest_prefix"}"
    ;;
  "$pipe_buffer_prefix"*)
    pipe_buffer_count=$((pipe_buffer_count + 1))
    pipe_buffer="${line#"$pipe_buffer_prefix"}"
    ;;
  *) ;;
  esac
done
[[ "$marker_count" == 1 ]]
[[ "$sentinel_count" == 1 ]]
[[ "$digest_count" == 1 ]]
[[ "$pipe_buffer_count" == 1 ]]
[[ "$sidecar_digest" =~ ^[0-9a-f]{64}$ ]]
[[ "$pipe_buffer" =~ ^[0-9]+$ ]]
resource_evidence="${resource_marker#"$resource_marker_prefix"}"
[[ "${#resource_evidence}" -le 32768 ]]
marker_bytes="$(printf '%s\n' "$resource_marker" | wc --bytes)"
((marker_bytes <= pipe_buffer))
calculated_digest="$(printf '%s\n' "$resource_evidence" | sha256sum | cut --delimiter=' ' --fields=1)"
[[ "$calculated_digest" == "$sidecar_digest" ]]
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
fixture_markers="$(run_fixture_with_captured_output "$fixture_script" "$fixture_sidecar")"
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
unavailable_logs="$(run_fixture_with_captured_output "$unavailable_script" "$unavailable_sidecar")"
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
