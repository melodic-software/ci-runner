#!/usr/bin/env bash
set -u

readonly cgroup_root="${1:-/sys/fs/cgroup}"
readonly state_directory=/home/runner/_runner_state
readonly final_path="$state_directory/cgroup-terminal.json"
readonly marker_prefix=ci-runner-resource-evidence-v1:
readonly maximum_evidence_bytes=32768
readonly container_stdout=/proc/1/fd/1

# Resource evidence is operational telemetry, not a security boundary. Workflow
# code shares the disposable runner identity, so refuse a replaced state path
# and leave controller-side fallback classification to the host.
if [[ ! -d "$state_directory" || -L "$state_directory" ]]; then
  exit 0
fi

missing=()
# Accept a decimal integer or record the field as missing. Shared by scalar
# files and key/value stat maps so the degrade path stays one shape.
take_numeric() {
  local name="$1" value="${2:-}"
  if [[ "$value" =~ ^[0-9]+$ ]]; then
    REPLY="$value"
    return
  fi
  missing+=("$name")
  REPLY=0
}

read_scalar() {
  local name="$1" path="$2" value
  if [[ -r "$path" ]]; then
    IFS= read -r value <"$path" || value=''
  else
    value=''
  fi
  take_numeric "$name" "$value"
}

# Load a whitespace key/value cgroup file into a nameref associative array.
# Bash read stays in-process; the previous per-key awk fork (five of them on
# the complete path) is the spawn-census hotspot this replaces. GNU Bash
# namerefs: https://www.gnu.org/software/bash/manual/html_node/Shell-Parameters.html
# Line iteration: https://mywiki.wooledge.org/BashFAQ/001
read_map() {
  local read_map_path="$1"
  local -n read_map_dest="$2"
  local read_map_key read_map_value
  # nameref: these writes update the caller associative array, not a local copy.
  # shellcheck disable=SC2034
  read_map_dest=()
  [[ -r "$read_map_path" ]] || return 0
  while read -r read_map_key read_map_value || [[ -n ${read_map_key:-} ]]; do
    [[ -n "$read_map_key" ]] || continue
    # shellcheck disable=SC2034
    read_map_dest["$read_map_key"]="$read_map_value"
  done <"$read_map_path"
}

read_scalar memory.peak "$cgroup_root/memory.peak"
memory_peak="$REPLY"
read_scalar memory.swap.peak "$cgroup_root/memory.swap.peak"
memory_swap_peak="$REPLY"

declare -A memory_events=()
read_map "$cgroup_root/memory.events" memory_events
take_numeric memory.events.oom "${memory_events[oom]:-}"
memory_oom="$REPLY"
take_numeric memory.events.oom_kill "${memory_events[oom_kill]:-}"
memory_oom_kill="$REPLY"

declare -A cpu_stat=()
read_map "$cgroup_root/cpu.stat" cpu_stat
take_numeric cpu.stat.nr_periods "${cpu_stat[nr_periods]:-}"
cpu_periods="$REPLY"
take_numeric cpu.stat.nr_throttled "${cpu_stat[nr_throttled]:-}"
cpu_throttled="$REPLY"
take_numeric cpu.stat.throttled_usec "${cpu_stat[throttled_usec]:-}"
cpu_throttled_usec="$REPLY"

read_scalar pids.peak "$cgroup_root/pids.peak"
pids_peak="$REPLY"

io_read_bytes=0
io_write_bytes=0
io_valid=0
if [[ -r "$cgroup_root/io.stat" ]]; then
  read -r io_read_bytes io_write_bytes io_valid < <(
    awk '
      {
        line_read = 0
        line_write = 0
        for (field = 2; field <= NF; field++) {
          if (split($field, pair, "=") != 2) invalid = 1
          if (pair[1] == "rbytes" && pair[2] ~ /^[0-9]+$/) {
            read_bytes += pair[2]
            line_read = 1
          }
          if (pair[1] == "wbytes" && pair[2] ~ /^[0-9]+$/) {
            write_bytes += pair[2]
            line_write = 1
          }
        }
        if (line_read && line_write) valid = 1
        else invalid = 1
      }
      END { printf "%.0f %.0f %d\n", read_bytes, write_bytes, (valid && !invalid) }
    ' "$cgroup_root/io.stat"
  )
  if [[ "$io_valid" != 1 ]]; then
    io_read_bytes=0
    io_write_bytes=0
    missing+=(io.stat)
  fi
else
  missing+=(io.stat)
fi

status=complete
if ((${#missing[@]} > 0)); then
  status=partial
fi
if ((${#missing[@]} == 9)); then
  status=unavailable
fi

umask 077
temporary="$(mktemp "$state_directory/.cgroup-terminal.XXXXXX")" || exit 0
trap 'rm --force "$temporary"' EXIT
# --args is jq's documented way to pass remaining argv as $ARGS.positional
# (https://jqlang.github.io/jq/manual/#invoking-jq). That replaces a nested
# jq --raw-input --slurp over printf, which spawn-census counted as a second
# jq on every path including unavailable.
if ! jq --compact-output --null-input \
  --arg status "$status" \
  --argjson memory_peak "$memory_peak" \
  --argjson memory_swap_peak "$memory_swap_peak" \
  --argjson memory_oom "$memory_oom" \
  --argjson memory_oom_kill "$memory_oom_kill" \
  --argjson cpu_periods "$cpu_periods" \
  --argjson cpu_throttled "$cpu_throttled" \
  --argjson cpu_throttled_usec "$cpu_throttled_usec" \
  --argjson pids_peak "$pids_peak" \
  --argjson io_read_bytes "$io_read_bytes" \
  --argjson io_write_bytes "$io_write_bytes" \
  --args \
  '{
    schemaVersion: 1,
    source: "cgroup-v2",
    status: $status,
    missing: $ARGS.positional,
    memory: {
      peakBytes: $memory_peak,
      swapPeakBytes: $memory_swap_peak,
      oomEvents: $memory_oom,
      oomKillEvents: $memory_oom_kill
    },
    cpu: {
      periods: $cpu_periods,
      throttledPeriods: $cpu_throttled,
      throttledMicroseconds: $cpu_throttled_usec
    },
    pids: { peak: $pids_peak },
    io: { readBytes: $io_read_bytes, writeBytes: $io_write_bytes }
  }' \
  -- "${missing[@]}" >"$temporary"; then
  exit 0
fi
evidence_size="$(wc --bytes <"$temporary")" || exit 0
if ((evidence_size < 1 || evidence_size > maximum_evidence_bytes)); then
  exit 0
fi
chmod 0600 "$temporary" || exit 0
mv --force "$temporary" "$final_path" || exit 0
trap - EXIT
if IFS= read -r evidence <"$final_path"; then
  marker="$marker_prefix$evidence"
  pipe_buffer="$(getconf PIPE_BUF "$container_stdout" 2>/dev/null)" || exit 0
  if [[ ! "$pipe_buffer" =~ ^[0-9]+$ ]] || ((${#marker} + 1 > pipe_buffer)); then
    exit 0
  fi
  # The pinned runner redirects completion-hook stdout/stderr into its job log
  # pipeline. PID 1 is the same-UID runner launcher for the container lifetime,
  # and its stdout is Docker's logging pipe. Open that pipe directly so the
  # controller can observe one PIPE_BUF-bounded atomic marker write.
  { printf '%s\n' "$marker" >"$container_stdout"; } 2>/dev/null || true
fi
exit 0
