const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const repositoryRoot = path.resolve(__dirname, "..", "..");
const workflowDirectory = path.join(repositoryRoot, ".github", "workflows");
const ciWorkflowsReference = "melodic-software/ci-workflows/";
const syncManagedMarker = /^#\s*SYNC-MANAGED FILE/mu;
const advisoryReference =
  /^\s*uses:\s+melodic-software\/ci-workflows\/[^\s@#]+@(?<sha>[0-9a-f]{40})\s+#\s+\S/;
const referenceAtMain =
  /^\s*uses:\s+melodic-software\/ci-workflows\/(?<reference>[^\s@#]+)@main(?:\s+#.*)?$/u;

// Which files call which ci-workflows composite or reusable, sorted, with
// duplicates kept. This is the omitted-caller regression check: a lane that
// quietly stops calling its shared action, or one that appears without review,
// fails here.
const expectedCiWorkflowsCallers = [
  ["pr-require-checks.yml", ".github/actions/actionlint"],
  ["pr-require-checks.yml", ".github/actions/check-comment-markers"],
  ["pr-require-checks.yml", ".github/actions/check-exec-bit"],
  ["pr-require-checks.yml", ".github/actions/check-jsonschema"],
  ["pr-require-checks.yml", ".github/actions/check-jsonschema"],
  ["pr-require-checks.yml", ".github/actions/check-line-endings"],
  ["pr-require-checks.yml", ".github/actions/check-machine-paths"],
  ["pr-require-checks.yml", ".github/actions/editorconfig-checker"],
  ["pr-require-checks.yml", ".github/actions/gitleaks"],
  ["pr-require-checks.yml", ".github/actions/lychee-offline"],
  ["pr-require-checks.yml", ".github/actions/markdownlint"],
  ["pr-require-checks.yml", ".github/actions/pr-require-checks/report-ci-status"],
  ["pr-require-checks.yml", ".github/actions/shellcheck"],
  ["pr-require-checks.yml", ".github/actions/shfmt"],
  ["pr-require-checks.yml", ".github/actions/typos"],
  ["pr-require-checks.yml", ".github/workflows/pr-audit-workflows.yml"],
  ["pr-require-checks.yml", ".github/workflows/pr-run-checks-go.yml"],
];

function workflowSource(name) {
  return fs.readFileSync(path.join(workflowDirectory, name), "utf8");
}

function jobBlock(source, name) {
  const lines = source.split(/\r?\n/u);
  const start = lines.findIndex((line) => line === `  ${name}:`);
  assert.notEqual(start, -1, `workflow job ${name} is missing`);
  const next = lines.findIndex(
    (line, index) => index > start && /^ {2}[A-Za-z0-9_-]+:\s*$/u.test(line),
  );
  return lines.slice(start + 1, next === -1 ? lines.length : next);
}

function directMappingKeys(lines) {
  return lines
    .map((line) => /^ {4}([A-Za-z0-9_-]+):(?:\s.*)?$/u.exec(line)?.[1])
    .filter((key) => key !== undefined);
}

function nestedMapping(lines, name) {
  const start = lines.findIndex((line) => line === `    ${name}:`);
  assert.notEqual(start, -1, `${name} mapping is missing`);
  const entries = [];
  for (const line of lines.slice(start + 1)) {
    if (/^ {4}\S/u.test(line)) {
      break;
    }
    const match = /^ {6}([A-Za-z0-9_-]+):\s*(\S.*)$/u.exec(line);
    if (match) {
      entries.push([match[1], match[2]]);
    }
  }
  return Object.fromEntries(entries);
}

function directList(lines, name) {
  const start = lines.findIndex((line) => line === `    ${name}:`);
  assert.notEqual(start, -1, `${name} list is missing`);
  const entries = [];
  for (const line of lines.slice(start + 1)) {
    if (/^ {4}\S/u.test(line)) {
      break;
    }
    const match = /^ {6}-\s+(\S+)$/u.exec(line);
    if (match) {
      entries.push(match[1]);
    }
  }
  return entries;
}

function workflowFiles(directory) {
  return fs
    .readdirSync(directory, { withFileTypes: true })
    .flatMap((entry) => {
      const entryPath = path.join(directory, entry.name);
      if (entry.isDirectory()) {
        return workflowFiles(entryPath);
      }
      return /\.ya?ml$/u.test(entry.name) ? [entryPath] : [];
    })
    .sort();
}

// Every ci-workflows reference runs at `@main`: first-party refs are not
// pinned. The test asserts the expected callers are all present and none is
// pinned. A SYNC-MANAGED file may still carry a SHA-pinned advisory reference
// until the standards sync moves it.
function ciWorkflowsCallers() {
  const callers = [];

  for (const file of workflowFiles(workflowDirectory)) {
    const source = fs.readFileSync(file, "utf8");
    const advisory = syncManagedMarker.test(source);
    const lines = source.split(/\r?\n/u);
    for (const [index, line] of lines.entries()) {
      // A full-line YAML comment can link a ci-workflows file; it calls nothing.
      if (!line.includes(ciWorkflowsReference) || /^\s*#/u.test(line)) {
        continue;
      }

      if (advisory) {
        assert.ok(
          advisoryReference.exec(line) || referenceAtMain.exec(line),
          `${path.relative(workflowDirectory, file)}:${index + 1} is not a SHA-pinned or main advisory ci-workflows reference`,
        );
        continue;
      }

      const match = referenceAtMain.exec(line);
      assert.ok(
        match,
        `${path.relative(workflowDirectory, file)}:${index + 1} does not run ci-workflows at main`,
      );
      callers.push([
        path.relative(workflowDirectory, file).split(path.sep).join("/"),
        match.groups.reference,
      ]);
    }
  }

  assert.deepEqual(
    callers.sort(),
    expectedCiWorkflowsCallers,
    "every lane and gate caller must stay inventoried; adding or removing one is a reviewed change",
  );
}

test("ci-workflows references run at main and every expected caller is present", () => {
  ciWorkflowsCallers();
});

test("pr-run-checks-go uses the exact reusable caller contract", () => {
  const block = jobBlock(workflowSource("pr-require-checks.yml"), "pr-run-checks-go");

  // `if` is admitted because the required-check contract gates every lane job
  // on the contract-only predicate, pr-run-checks-go included: a `labeled`,
  // `unlabeled` or `edited` event cannot change any Go result, so the lane is
  // skipped and ci-status carries the recorded `ci-lanes` verdict forward. The
  // predicate is asserted below so the caller cannot smuggle in an arbitrary
  // condition. `name` is the display name the org naming convention requires
  // on every job; every other key stays closed.
  assert.deepEqual(
    directMappingKeys(block),
    ["name", "if", "permissions", "uses", "with"],
    "pr-run-checks-go must not add a runner, caller secrets, or undeclared inputs",
  );
  assert.ok(
    block.includes(
      "    if: ${{ !(github.event.pull_request.head.repo.full_name == github.repository && (contains(fromJSON('[\"labeled\",\"unlabeled\"]'), github.event.action) || (github.event.action == 'edited' && !github.event.changes.base))) }}",
    ),
    "pr-run-checks-go's only condition is the contract-only gate",
  );
  assert.deepEqual(nestedMapping(block, "permissions"), { contents: "read" });
  assert.deepEqual(nestedMapping(block, "with"), { config: ".golangci.yml" });
  assert.ok(
    block.includes(
      "    uses: melodic-software/ci-workflows/.github/workflows/pr-run-checks-go.yml@main",
    ),
    "pr-run-checks-go must call the first-party reusable workflow at main",
  );
});

test("local Go jobs do not duplicate reusable quality checks", () => {
  const source = workflowSource("pr-require-checks.yml");
  assert.doesNotMatch(source, /^ {2}go:\s*$/mu);
  assert.doesNotMatch(source, /^ {2}go-windows:\s*$/mu);

  const crossBuild = jobBlock(source, "build-go-windows").join("\n");
  assert.match(crossBuild, /^ {4}runs-on: ubuntu-24\.04$/mu);
  assert.match(crossBuild, /^ {10}GOOS: windows$/mu);
  assert.equal((crossBuild.match(/^ {10}go build /gmu) ?? []).length, 2);
  for (const duplicate of ["go test", "go vet", "go mod", "govulncheck", "golangci-lint"]) {
    assert.ok(!crossBuild.includes(duplicate), `cross-build must not duplicate ${duplicate}`);
  }

  const fuzz = jobBlock(source, "fuzz-go").join("\n");
  assert.match(
    fuzz,
    /^ {4}if: github\.event_name == 'schedule' \|\| github\.event_name == 'workflow_dispatch'$/mu,
  );
  assert.equal((fuzz.match(/-fuzztime=30s/gmu) ?? []).length, 2);
});

test("ci-status is the stable gateway for every Go lane", () => {
  const block = jobBlock(workflowSource("pr-require-checks.yml"), "ci-status");
  const expectedNeeds = [
    "markdownlint",
    "shellcheck",
    "shfmt",
    "typos",
    "editorconfig-checker",
    "gitleaks",
    "lychee",
    "check-comment-markers",
    "actionlint",
    "jsonschema",
    "check-exec-bit",
    "check-machine-paths",
    "check-line-endings",
    "pr-audit-workflows",
    "test-policy-contracts",
    "pr-run-checks-go",
    "build-go-windows",
    "fuzz-go",
    "build-worker-image",
    "review-dependencies",
  ];
  assert.deepEqual(directList(block, "needs"), expectedNeeds);

  const results = block
    .map((line) => /\$\{\{ needs\.([A-Za-z0-9_-]+)\.result \}\}/u.exec(line)?.[1])
    .filter((name) => name !== undefined);
  assert.deepEqual(results, expectedNeeds, "ci-status must aggregate every declared dependency once");
  assert.ok(block.includes("    name: CI Status"));
  assert.ok(block.includes("    if: always()"));
});

// report-ci-status never fails on the contract or the draft policy; it reports
// them as outputs. Without this step a failed contract leaves CI Status green.
test("ci-status fails closed on the report-ci-status contract and draft outputs", () => {
  const block = jobBlock(workflowSource("pr-require-checks.yml"), "ci-status");
  const report = block.findIndex((line) => line === "        id: report");
  assert.notEqual(report, -1, "the report-ci-status step must carry id: report");
  assert.equal(block[report - 1], "      - name: Aggregate lane results", "the writer step keeps its name");

  const gate = block.findIndex(
    (line) =>
      line ===
      "        if: ${{ !cancelled() && (steps.report.outputs.contract != 'success' || steps.report.outputs.draft != 'success') }}",
  );
  assert.ok(gate > report, "a later step must fail the job on any contract or draft outcome but success");
  assert.ok(block.slice(gate).some((line) => line.trim() === "exit 1"), "the gate step must exit 1");
});
