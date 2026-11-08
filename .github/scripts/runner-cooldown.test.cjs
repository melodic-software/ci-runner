"use strict";

const assert = require("node:assert/strict");
const {join, resolve} = require("node:path");
const {spawnSync} = require("node:child_process");
const test = require("node:test");

const pinSources = join(resolve(__dirname, "..", ".."), "scripts", "ReleasePinSources.ps1");
const now = Date.parse("2026-11-20T12:00:00Z");
const day = 24 * 60 * 60 * 1000;

function release(version, ageDays, body = "") {
  return {
    tag_name: `v${version}`,
    published_at: new Date(now - ageDays * day).toISOString(),
    body,
    draft: false,
    prerelease: false,
    html_url: `https://github.com/actions/runner/releases/tag/v${version}`,
  };
}

// Calls Get-RunnerAdoptionCandidates with the repository defaults (3 days, 7 for a major).
function candidates(pinned, releases) {
  const command = [
    ". $env:PIN_SOURCES",
    "$releases = @($env:RELEASES | ConvertFrom-Json)",
    "$result = Get-RunnerAdoptionCandidates $releases $env:PINNED ([DateTimeOffset]$env:NOW) 3 7",
    "[ordered]@{ critical = $result.critical; eligible = @($result.eligible | ForEach-Object { $_.tag_name.TrimStart('v') }); held = @($result.held) } | ConvertTo-Json -Depth 4 -Compress",
  ].join("\n");
  const run = spawnSync("pwsh", ["-NoProfile", "-NonInteractive", "-Command", command], {
    encoding: "utf8",
    env: {
      ...process.env,
      PIN_SOURCES: pinSources,
      RELEASES: JSON.stringify(releases),
      PINNED: pinned,
      NOW: new Date(now).toISOString(),
    },
  });
  assert.equal(run.status, 0, run.stderr);
  return JSON.parse(run.stdout);
}

test("a patch or minor release is held for 3 days", () => {
  const result = candidates("2.338.2", [release("2.338.2", 30), release("2.338.3", 2.9), release("2.339.0", 1)]);
  assert.equal(result.critical, false);
  assert.deepEqual(result.eligible, ["2.338.2"]);
  assert.deepEqual(result.held.map((held) => [held.version, held.cooldownDays]), [["2.338.3", 3], ["2.339.0", 3]]);
});

test("a patch release becomes eligible at exactly 3 days", () => {
  const result = candidates("2.338.2", [release("2.338.2", 30), release("2.338.3", 3)]);
  assert.deepEqual(result.eligible, ["2.338.2", "2.338.3"]);
  assert.deepEqual(result.held, []);
});

test("a major release is held for 7 days and eligible at exactly 7", () => {
  const held = candidates("2.338.2", [release("2.338.2", 30), release("3.0.0", 6.9)]);
  assert.deepEqual(held.eligible, ["2.338.2"]);
  assert.equal(held.held[0].cooldownDays, 7);
  assert.equal(Date.parse(held.held[0].eligibleAt), now + 0.1 * day);

  const eligible = candidates("2.338.2", [release("2.338.2", 30), release("3.0.0", 7)]);
  assert.deepEqual(eligible.eligible, ["2.338.2", "3.0.0"]);
});

test("the newest release past its cooldown is the target while a younger one is held", () => {
  const result = candidates("2.338.2", [
    release("2.338.2", 30),
    release("2.339.0", 10),
    release("2.340.0", 1),
  ]);
  assert.deepEqual(result.eligible, ["2.338.2", "2.339.0"]);
  assert.deepEqual(result.held.map((held) => held.version), ["2.340.0"]);
});

test("a critical or CVE release skips its own cooldown and no other release's", () => {
  for (const body of ["Fixes a critical bug", "Addresses CVE-2026-12345"]) {
    const result = candidates("2.338.2", [release("2.338.2", 30), release("2.338.3", 1, body), release("3.0.0", 1)]);
    assert.equal(result.critical, true);
    assert.deepEqual(result.eligible, ["2.338.2", "2.338.3"]);
    assert.deepEqual(result.held.map((held) => held.version), ["3.0.0"]);
  }
});

test("a critical note on the pinned or an older release does not lift the cooldown", () => {
  const result = candidates("2.338.2", [release("2.338.1", 40, "security fix"), release("2.338.2", 30), release("2.338.3", 1)]);
  assert.equal(result.critical, false);
  assert.deepEqual(result.eligible, ["2.338.1", "2.338.2"]);
});
