"use strict";

const assert = require("node:assert/strict");
const {chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} = require("node:fs");
const {tmpdir} = require("node:os");
const {delimiter, join, resolve} = require("node:path");
const {spawnSync} = require("node:child_process");
const test = require("node:test");

const script = join(resolve(__dirname, "..", ".."), "scripts", "Update-DependencyDriftIssue.ps1");
const title = "Dependency freshness: drift at or past the promotion target";
const runUrl = "https://github.com/melodic-software/ci-runner/actions/runs/1";

// A `gh` stand-in: logs each call's arguments (and any --body-file content)
// and answers `issue list` with FAKE_ISSUES.
const fakeGh = `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"$FAKE_LOG"
if [[ "$1 $2" == "issue list" ]]; then
  printf '%s\\n' "$FAKE_ISSUES"
fi
while (($#)); do
  if [[ "$1" == --body-file ]]; then
    cp "$2" "$FAKE_BODY"
  fi
  shift
done
`;

function drift(dependency, ageDays, extra = {}) {
  return {
    dependency,
    pinned: "1.0.0",
    latest: "1.1.0",
    publishedAt: "2026-11-01T00:00:00.0000000+00:00",
    ageDays,
    critical: false,
    hardFail: ageDays >= 14,
    source: `https://example.test/${dependency}`,
    ...extra,
  };
}

function run(rows, issues = []) {
  const directory = mkdtempSync(join(tmpdir(), "drift-issue-"));
  try {
    const bin = join(directory, "bin");
    const gh = join(bin, "gh");
    mkdirSync(bin);
    writeFileSync(gh, fakeGh);
    chmodSync(gh, 0o755);
    const report = join(directory, "report.json");
    writeFileSync(report, JSON.stringify({
      schemaVersion: 1,
      checkedAt: "2026-11-08T06:41:00.0000000+00:00",
      hardFailAfterDays: 14,
      hasDrift: rows.length > 0,
      hardFail: rows.some((row) => row.hardFail),
      drift: rows,
      cooldownHeld: [],
    }));
    const review = join(directory, "review.json");
    writeFileSync(review, JSON.stringify({held: [{dependency: "held-pin"}]}));
    const log = join(directory, "gh.log");
    const body = join(directory, "body.md");
    const result = spawnSync("pwsh", [
      "-NoProfile", "-NonInteractive", "-File", script,
      "-ReportPath", report, "-ReviewPath", review, "-RunUrl", runUrl,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${bin}${delimiter}${process.env.PATH}`,
        FAKE_LOG: log,
        FAKE_BODY: body,
        FAKE_ISSUES: JSON.stringify(issues),
        GH_REPO: "melodic-software/ci-runner",
      },
    });
    assert.equal(result.status, 0, result.stderr + result.stdout);
    const read = (path) => {
      try {
        return readFileSync(path, "utf8");
      } catch {
        return "";
      }
    };
    return {calls: read(log).trim().split("\n"), body: read(body), stdout: result.stdout};
  } finally {
    rmSync(directory, {recursive: true, force: true});
  }
}

test("drift at the 7-day target opens a labeled issue listing every row", () => {
  const {calls, body} = run([drift("young", 6), drift("aged", 7), drift("held-pin", 15)]);
  assert.equal(calls.length, 2);
  assert.match(calls[1], new RegExp(`^issue create --title ${title} --body-file \\S+ --label automated --label dependencies$`, "u"));
  assert.match(body, /found 2 of 3 drift rows at or past the 7-day promotion target/u);
  assert.match(body, /A release tag pushed now fails the release freshness gate/u);
  assert.match(body, /Checked at 2026-11-08T06:41:00\.0000000\+00:00 by https:\/\/github\.com\/melodic-software\/ci-runner\/actions\/runs\/1\./u);
  const table = body.split("\n").filter((line) => line.startsWith("| ") && !line.startsWith("| Dependency") && !line.startsWith("| ---"));
  assert.deepEqual(table.map((line) => line.split(" | ").slice(0, 1).concat(line.split(" | ").slice(4, 6))), [
    ["| held-pin", "hard fail", "yes"],
    ["| aged", "past target", "no"],
    ["| young", "within target", "no"],
  ]);
  assert.match(body, /\(https:\/\/github\.com\/melodic-software\/ci-runner\/blob\/main\/docs\/releases\.md#freshness-policy\)/u);
});

test("an integrity row hard-fails at age 0 and updates the open issue in place", () => {
  const issues = [{number: 12, title: "Unrelated"}, {number: 40, title}];
  const {calls, body} = run([drift("digest", 0, {critical: true, hardFail: true, pinned: "a|b"})], issues);
  assert.equal(calls.length, 2);
  assert.match(calls[1], /^issue edit 40 --body-file \S+$/u);
  assert.match(body, /\| digest \| a\\\|b \| 1\.1\.0 \| 0 \| hard fail \(critical\) \| no \|/u);
});

test("drift below the target closes the open issue with the run link", () => {
  const {calls, body} = run([drift("young", 6)], [{number: 40, title}]);
  assert.deepEqual(calls.slice(1), [
    `issue close 40 --comment The scheduled check found no drift at or past the 7-day promotion target: ${runUrl}`,
  ]);
  assert.equal(body, "");
});

test("a clean report with no open issue writes nothing", () => {
  const {calls, stdout} = run([]);
  assert.equal(calls.length, 1);
  assert.match(calls[0], /^issue list --state open --label automated /u);
  assert.match(stdout, /no issue to close/u);
});
