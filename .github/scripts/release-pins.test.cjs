"use strict";

const assert = require("node:assert/strict");
const {cpSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync} = require("node:fs");
const {tmpdir} = require("node:os");
const {join, resolve} = require("node:path");
const {spawnSync} = require("node:child_process");
const test = require("node:test");

const repositoryRoot = resolve(__dirname, "..", "..");
const checkout = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1";

// Runs Test-ReleasePins.ps1 against a copy of the repository after `edit`
// rewrites it the way a Dependabot bump would, leaving release/ untouched.
function checkPins(edit) {
  const root = mkdtempSync(join(tmpdir(), "release-pins-"));
  try {
    for (const path of ["Dockerfile", "README.md", "go.mod", "docs", "release", "scripts", ".github"]) {
      cpSync(join(repositoryRoot, path), join(root, path), {recursive: true});
    }
    edit(root);
    // Print the bare exception message: pwsh's error view wraps and colors it.
    const command = "try { & $env:PINS_SCRIPT -RepositoryRoot $env:PINS_ROOT } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }";
    return spawnSync("pwsh", ["-NoProfile", "-NonInteractive", "-Command", command], {
      encoding: "utf8",
      env: {...process.env, PINS_SCRIPT: join(root, "scripts", "Test-ReleasePins.ps1"), PINS_ROOT: root},
    });
  } finally {
    rmSync(root, {recursive: true, force: true, maxRetries: 3, retryDelay: 100});
  }
}

function rewrite(path, from, to) {
  const source = readFileSync(path, "utf8");
  assert.ok(source.includes(from), `${path} does not contain ${from}`);
  writeFileSync(path, source.replaceAll(from, to));
}

function workflows(root) {
  const directory = join(root, ".github", "workflows");
  return readdirSync(directory)
    .filter((name) => name.endsWith(".yml"))
    .map((name) => join(directory, name));
}

function workflowsUsingCheckout(root) {
  return workflows(root).filter((path) => readFileSync(path, "utf8").includes(checkout));
}

test("a grouped Action bump passes without a manifest edit", () => {
  const result = checkPins((root) => {
    for (const path of workflowsUsingCheckout(root)) {
      rewrite(path, checkout, `actions/checkout@${"a".repeat(40)} # v7.0.2`);
    }
  });
  assert.equal(result.status, 0, result.stderr + result.stdout);
});

test("a runner image bump passes without a manifest edit", () => {
  const result = checkPins((root) => {
    rewrite(
      join(root, "Dockerfile"),
      "FROM ghcr.io/actions/actions-runner:2.338.2@sha256:759a1d94a15b05941aba5196974ff43fe15b4cbba29b694b35cbc32a1166b232",
      `FROM ghcr.io/actions/actions-runner:2.339.0@sha256:${"b".repeat(64)}`,
    );
  });
  assert.equal(result.status, 0, result.stderr + result.stdout);
});

test("uses of one Action that name different releases fail with their locations", () => {
  const result = checkPins((root) => {
    rewrite(workflowsUsingCheckout(root)[0], checkout, `actions/checkout@${"a".repeat(40)} # v7.0.2`);
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr + result.stdout, /\.yml:\d+ pins actions\/checkout@[0-9a-f]{40} # v7\.0\.\d, but \S+\.yml:\d+ pins/u);
});

test("an Action pin without a release comment fails with its location", () => {
  const result = checkPins((root) => {
    rewrite(workflowsUsingCheckout(root)[0], checkout, "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1");
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr + result.stdout, /\.yml:\d+ pins actions\/checkout without a same-line '# vX\.Y\.Z' release comment/u);
});
