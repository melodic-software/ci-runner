#!/usr/bin/env bash
# Cloud bootstrap: install the plugin catalog this repo enables. Two callers:
#   1. The account environment's setup script, after clone and before the
#      session process launches. Claude Code builds its plugin registry at
#      process start and never re-reads it, so this pre-launch call is the
#      only path that gets plugins loaded at turn one.
#   2. The SessionStart hook (startup|resume), as per-session drift repair —
#      the environment cache can be ~7 days stale. Plugins the hook installs
#      go live at the next resume.
# Declaring a marketplace is gated on workspace trust and cloud sessions arrive
# untrusted, so the declaration alone can load nothing there. Hooks run untrusted.
# Idempotent and best effort: a failed plugin costs its skills, not the session.
set -Eeuo pipefail

# Cloud sessions only: local sessions load plugins through workspace trust and
# the marketplace declaration in settings.json, so there is nothing to do.
[[ "${CLAUDE_CODE_REMOTE:-}" == "true" ]] || exit 0

repo_root="${CLAUDE_PROJECT_DIR:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)}"
cd -- "$repo_root"

command -v claude >/dev/null 2>&1 || exit 0
command -v jq >/dev/null 2>&1 || exit 0

marketplace="melodic-software"
source_repo="melodic-software/claude-code-plugins"

if ! claude plugin marketplace list --json 2>/dev/null |
  jq -e --arg n "$marketplace" 'any(.[]; .name == $n)' >/dev/null; then
  claude plugin marketplace add "$source_repo" --scope user >/dev/null || {
    echo "cloud-bootstrap: could not add the $marketplace marketplace" >&2
    exit 0
  }
fi

mapfile -t wanted < <(
  jq -r --arg n "$marketplace" \
    '.enabledPlugins // {} | to_entries[]
     | select(.value == true and (.key | endswith("@" + $n))) | .key' \
    .claude/settings.json 2>/dev/null
)
mapfile -t have < <(claude plugin list --json 2>/dev/null | jq -r '.[].id' 2>/dev/null)

installed=0
for id in "${wanted[@]}"; do
  [[ -n "$id" ]] || continue
  if [[ " ${have[*]} " == *" $id "* ]]; then continue; fi
  if claude plugin install "$id" --scope user -y >/dev/null 2>&1; then
    installed=$((installed + 1))
  else
    echo "cloud-bootstrap: install failed: $id" >&2
  fi
done

# `claude plugin install` does not activate a plugin in the session that runs it,
# so a first session would otherwise start without the catalog it just installed.
# reloadSkills re-scans the skill and command directories once SessionStart hooks
# finish, which activates the skills and commands in this same session. Ask for it
# only when something was installed; on a warm start the plugins already loaded.
reload=false
if [[ $installed -gt 0 ]]; then reload=true; fi

jq -n --argjson reload "$reload" \
  --arg summary "cloud-bootstrap: ${#wanted[@]} enabled, $installed newly installed" \
  '{hookSpecificOutput: {
      hookEventName: "SessionStart",
      reloadSkills: $reload,
      additionalContext: $summary
    }}'
