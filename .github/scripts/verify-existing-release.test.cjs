"use strict";

const assert = require("node:assert/strict");
const {chmodSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync} = require("node:fs");
const {tmpdir} = require("node:os");
const {join, resolve} = require("node:path");
const {spawnSync} = require("node:child_process");
const test = require("node:test");

const script = resolve(__dirname, "verify-existing-release.sh");

function writeShim(directory, name, content) {
  const path = join(directory, name);
  writeFileSync(path, `#!/usr/bin/env bash\nset -Eeuo pipefail\n${content}\n`);
  chmodSync(path, 0o755);
}

function run(mode) {
  const directory = mkdtempSync(join(tmpdir(), "verify-existing-release-"));
  const bin = join(directory, "bin");
  const output = join(directory, "output");
  mkdirSync(bin);
  writeFileSync(output, "");
  writeShim(bin, "gh", `
case "\${1:-} \${2:-}" in
  "api --include")
    case "\${FAKE_GH_MODE}" in
      absent) printf 'HTTP/2.0 404 Not Found\\n\\n'; exit 1 ;;
      body404) echo 'release not found (HTTP 404)' >&2; exit 1 ;;
      spoofed404) printf 'HTTP/2.0 403 Forbidden\n\nHTTP/2.0 404 Not Found\n'; exit 1 ;;
      failure) printf 'HTTP/2.0 403 Forbidden\\n\\n'; exit 1 ;;
      success) printf 'HTTP/2.0 200 OK\\n\\n{}\\n' ;;
    esac
    ;;
  "release view")
    printf '%s\\n' SHA256SUMS "ci-runner-\${RELEASE_VERSION}-windows-amd64.zip" "ci-runner-\${RELEASE_VERSION}-windows-amd64.spdx.json" compatibility.json
    ;;
  "release download")
    shift 2
    destination=''
    pattern=''
    while (($#)); do
      case "$1" in
        --dir) destination="$2"; shift 2 ;;
        --pattern) pattern="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    if [[ "$pattern" == 'SHA256SUMS' ]]; then
      (cd "$destination" && for file in "ci-runner-\${RELEASE_VERSION}-windows-amd64.zip" "ci-runner-\${RELEASE_VERSION}-windows-amd64.spdx.json" compatibility.json; do
        digest="$(sha256sum "$file" | cut --delimiter=' ' --fields=1)"
        printf '%s  %s\\n' "$digest" "$file"
      done >SHA256SUMS)
    else
      printf '%s\\n' "$pattern" >"$destination/$pattern"
    fi
    ;;
  *) ;;
esac`);
  writeShim(bin, "node", ":");
  writeShim(bin, "go", ":");
  writeShim(bin, "jq", `
for argument in "$@"; do
  if [[ "$argument" == '.worker.digest' ]]; then
    printf 'sha256:%064d\\n' 0
    exit 0
  fi
done
exit 0`);

  const result = spawnSync(process.env.BASH || "bash", [script], {
    cwd: directory,
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      FAKE_GH_MODE: mode,
      GH_TOKEN: "test-token",
      RELEASE_VERSION: "v1.2.3",
      GITHUB_OUTPUT: output,
      GITHUB_REPOSITORY: "melodic-software/ci-runner",
      GITHUB_REF: "refs/tags/v1.2.3",
      GITHUB_SHA: "a".repeat(40),
    },
  });
  const outputs = readFileSync(output, "utf8");
  rmSync(directory, {recursive: true, force: true, maxRetries: 3, retryDelay: 100});
  return {...result, outputs};
}

test("reports an absent release only for an explicit HTTP 404 status line", () => {
  const result = run("absent");
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.outputs, "exists=false\n");
});

test("does not trust a human-readable 404 diagnostic", () => {
  const result = run("body404");
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Unable to distinguish an absent release/);
  assert.equal(result.outputs, "");
});

test("classifies only the first HTTP status line", () => {
  const result = run("spoofed404");
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Unable to distinguish an absent release/);
  assert.equal(result.outputs, "");
});

test("fails closed when the release lookup fails ambiguously", () => {
  const result = run("failure");
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /Unable to distinguish an absent release/);
  assert.equal(result.outputs, "");
});

test("verifies exact existing evidence and returns its worker digest", () => {
  const result = run("success");
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.outputs, /^exists=true$/m);
  assert.match(result.outputs, /^worker_digest=sha256:[0-9a-f]{64}$/m);
});
