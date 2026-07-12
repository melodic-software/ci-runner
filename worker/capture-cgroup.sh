#!/usr/bin/env bash
set -u

readonly cgroup_root="${1:-/sys/fs/cgroup}"
readonly state_directory=/home/runner/_runner_state
readonly final_path="$state_directory/cgroup-terminal.json"

# Resource evidence is operational telemetry, not a security boundary. Workflow
# code shares the disposable runner identity, so refuse a replaced state path
# and leave controller-side fallback classification to the host.
if [[ ! -d "$state_directory" || -L "$state_directory" ]]; then
  exit 0
fi

missing=()
read_scalar() {
  local name="$1" path="$2" value
  if [[ -r "$path" ]]; then
    IFS= read -r value <"$path" || value=''
  else
    value=''
  fi
  if [[ "$value" =~ ^[0-9]+$ ]]; then
    REPLY="$value"
    return
  fi
  missing+=("$name")
  REPLY=0
}

read_stat() {
  local name="$1" path="$2" key="$3" value
  value=''
  if [[ -r "$path" ]]; then
    value="$(awk -v key="$key" '$1 == key && $2 ~ /^[0-9]+$/ { print $2; exit }' "$path")"
  fi
  if [[ "$value" =~ ^[0-9]+$ ]]; then
    REPLY="$value"
    return
  fi
  missing+=("$name")
  REPLY=0
}

read_scalar memory.peak "$cgroup_root/memory.peak"
memory_peak="$REPLY"
read_scalar memory.swap.peak "$cgroup_root/memory.swap.peak"
memory_swap_peak="$REPLY"
read_stat memory.events.oom "$cgroup_root/memory.events" oom
memory_oom="$REPLY"
read_stat memory.events.oom_kill "$cgroup_root/memory.events" oom_kill
memory_oom_kill="$REPLY"
read_stat cpu.stat.nr_periods "$cgroup_root/cpu.stat" nr_periods
cpu_periods="$REPLY"
read_stat cpu.stat.nr_throttled "$cgroup_root/cpu.stat" nr_throttled
cpu_throttled="$REPLY"
read_stat cpu.stat.throttled_usec "$cgroup_root/cpu.stat" throttled_usec
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
if ! jq --null-input \
  --arg status "$status" \
  --argjson missing "$(printf '%s\n' "${missing[@]}" | jq --raw-input --slurp 'split("\n") | map(select(length > 0))')" \
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
  '{
    schemaVersion: 1,
    source: "cgroup-v2",
    status: $status,
    missing: $missing,
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
  }' >"$temporary"; then
  exit 0
fi
chmod 0600 "$temporary" || exit 0
mv --force "$temporary" "$final_path" || exit 0
trap - EXIT
exit 0
