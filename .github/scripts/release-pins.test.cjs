"use strict";

const assert = require("node:assert/strict");
const {cpSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync} = require("node:fs");
const {tmpdir} = require("node:os");
const {join, resolve} = require("node:path");
const {spawnSync} = require("node:child_process");
const test = require("node:test");

const repositoryRoot = resolve(__dirname, "..", "..");
const workflowDirectory = join(repositoryRoot, ".github", "workflows");

// The pins are read from the repository so the tests survive the bumps they model.
const checkout = readdirSync(workflowDirectory)
  .filter((name) => name.endsWith(".yml"))
  .map((name) => /actions\/checkout@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+/u.exec(readFileSync(join(workflowDirectory, name), "utf8"))?.[0])
  .find((reference) => reference !== undefined);
const bumpedCheckout = `actions/checkout@${"a".repeat(40)} # v999.0.0`;
const runnerFrom = /^FROM ghcr\.io\/actions\/actions-runner:\S+$/mu.exec(readFileSync(join(repositoryRoot, "Dockerfile"), "utf8"))?.[0];

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
  const paths = workflows(root).filter((path) => readFileSync(path, "utf8").includes(checkout));
  assert.ok(paths.length > 1, "expected actions/checkout in more than one workflow");
  return paths;
}

test("a grouped Action bump passes without a manifest edit", () => {
  const result = checkPins((root) => {
    for (const path of workflowsUsingCheckout(root)) {
      rewrite(path, checkout, bumpedCheckout);
    }
  });
  assert.equal(result.status, 0, result.stderr + result.stdout);
});

test("a runner image bump passes without a manifest edit", () => {
  const result = checkPins((root) => {
    rewrite(join(root, "Dockerfile"), runnerFrom, `FROM ghcr.io/actions/actions-runner:999.0.0@sha256:${"b".repeat(64)}`);
  });
  assert.equal(result.status, 0, result.stderr + result.stdout);
});

test("uses of one Action that name different releases fail with their locations", () => {
  const result = checkPins((root) => {
    rewrite(workflowsUsingCheckout(root)[0], checkout, bumpedCheckout);
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr + result.stdout, /\.yml:\d+ pins actions\/checkout@[0-9a-f]{40} # v[0-9.]+, but \S+\.yml:\d+ pins/u);
});

test("an Action pin without a release comment fails with its location", () => {
  const result = checkPins((root) => {
    rewrite(workflowsUsingCheckout(root)[0], checkout, checkout.replace(/ # v\S+$/u, ""));
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr + result.stdout, /\.yml:\d+ pins actions\/checkout without a same-line '# vX\.Y\.Z' release comment/u);
});
