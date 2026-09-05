const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const repositoryRoot = path.resolve(__dirname, "..", "..");
const workflowDirectory = path.join(repositoryRoot, ".github", "workflows");
const ciWorkflowsReference = "melodic-software/ci-workflows/";
const syncManagedMarker = "SYNC-MANAGED FILE";
const canonicalReference =
  /^\s*uses:\s+melodic-software\/ci-workflows\/[^\s@#]+@(?<sha>[0-9a-f]{40})\s+#\s+(?<version>v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))\s*$/;
const advisoryReference =
  /^\s*uses:\s+melodic-software\/ci-workflows\/[^\s@#]+@(?<sha>[0-9a-f]{40})\s+#\s+\S/;

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

// The reviewed ci-workflows pin is whatever the canonical references agree on,
// not a constant repeated here. A grouped Dependabot bump rewrites every
// canonical `uses:` line at once and cannot touch this file or
// `release/dependencies.json`, so hardcoding the SHA, the version and the
// reference count made every bump red by construction and cost three manual
// edits. The tests below assert the property that matters instead: the
// references agree with each other, and the release manifest agrees with them.
// That leaves `release/dependencies.json` as the single manual edit per bump.
function ciWorkflowsPin() {
  const references = [];
  const versions = [];

  for (const file of workflowFiles(workflowDirectory)) {
    const source = fs.readFileSync(file, "utf8");
    const advisory = source.includes(syncManagedMarker);
    const lines = source.split(/\r?\n/u);
    for (const [index, line] of lines.entries()) {
      if (!line.includes(ciWorkflowsReference)) {
        continue;
      }

      // SYNC-MANAGED soak callers may pin a pre-release SHA with the
      // fallback comment form until a reviewed release carries the action.
      // They stay out of the one-release compatibility inventory so a
      // standards-sync cannot break Product policy; they still require a
      // full 40-hex SHA (https://docs.github.com/en/actions/reference/security/secure-use#using-third-party-actions).
      if (advisory) {
        assert.ok(
          advisoryReference.exec(line),
          `${path.relative(workflowDirectory, file)}:${index + 1} is not a SHA-pinned advisory ci-workflows reference`,
        );
        continue;
      }

      const match = canonicalReference.exec(line);
      assert.ok(
        match,
        `${path.relative(workflowDirectory, file)}:${index + 1} is not a canonical ci-workflows reference`,
      );
      references.push(match.groups.sha);
      versions.push(match.groups.version);
    }
  }

  assert.ok(
    references.length > 0,
    "the lane, link-check and gate callers must keep at least one canonical ci-workflows reference inventoried",
  );
  assert.equal(
    new Set(references).size,
    1,
    "ci-workflows references must move as one reviewed compatibility pin",
  );
  assert.equal(
    new Set(versions).size,
    1,
    "ci-workflows references must name one release version for online pin verification",
  );

  return { sha: references[0], version: versions[0], count: references.length };
}

test("ci-workflows references use a full SHA with one release version", () => {
  ciWorkflowsPin();
});

test("go-quality uses the exact reusable caller contract", () => {
  const { sha, version } = ciWorkflowsPin();
  const block = jobBlock(workflowSource("ci.yml"), "go-quality");

  // `if` is admitted because the required-check contract gates every lane job
  // on the contract-only predicate, go-quality included: a `labeled`,
  // `unlabeled` or `edited` event cannot change any Go result, so the lane is
  // skipped and ci-status carries the recorded `ci-lanes` verdict forward. The
  // predicate is asserted below so the caller cannot smuggle in an arbitrary
  // condition; every other key stays closed.
  assert.deepEqual(
    directMappingKeys(block),
    ["if", "permissions", "uses", "with"],
    "go-quality must not add a runner, caller secrets, or undeclared inputs",
  );
  assert.ok(
    block.includes(
      "    if: ${{ !(github.event.pull_request.head.repo.full_name == github.repository && (contains(fromJSON('[\"labeled\",\"unlabeled\"]'), github.event.action) || (github.event.action == 'edited' && !github.event.changes.base))) }}",
    ),
    "go-quality's only condition is the contract-only gate",
  );
  assert.deepEqual(nestedMapping(block, "permissions"), { contents: "read" });
  assert.deepEqual(nestedMapping(block, "with"), { config: ".golangci.yml" });
  assert.ok(
    block.includes(
      `    uses: melodic-software/ci-workflows/.github/workflows/go-quality.yml@${sha} # ${version}`,
    ),
    "go-quality must call the exact released reusable workflow every other reference names",
  );
});

test("release metadata tracks the same ci-workflows release", () => {
  const { sha } = ciWorkflowsPin();
  const dependencies = JSON.parse(
    fs.readFileSync(path.join(repositoryRoot, "release", "dependencies.json"), "utf8"),
  );
  assert.deepEqual(
    dependencies.repositoryPins.filter(
      ({ repository }) => repository === "melodic-software/ci-workflows",
    ),
    [
      {
        repository: "melodic-software/ci-workflows",
        commit: sha,
        source: `https://github.com/melodic-software/ci-workflows/tree/${sha}`,
      },
    ],
  );
});

test("local Go jobs do not duplicate reusable quality checks", () => {
  const source = workflowSource("ci.yml");
  assert.doesNotMatch(source, /^ {2}go:\s*$/mu);
  assert.doesNotMatch(source, /^ {2}go-windows:\s*$/mu);

  const crossBuild = jobBlock(source, "go-windows-build").join("\n");
  assert.match(crossBuild, /^ {4}runs-on: ubuntu-24\.04$/mu);
  assert.match(crossBuild, /^ {10}GOOS: windows$/mu);
  assert.equal((crossBuild.match(/^ {10}go build /gmu) ?? []).length, 2);
  for (const duplicate of ["go test", "go vet", "go mod", "govulncheck", "golangci-lint"]) {
    assert.ok(!crossBuild.includes(duplicate), `cross-build must not duplicate ${duplicate}`);
  }

  const fuzz = jobBlock(source, "go-fuzz").join("\n");
  assert.match(
    fuzz,
    /^ {4}if: github\.event_name == 'schedule' \|\| github\.event_name == 'workflow_dispatch'$/mu,
  );
  assert.equal((fuzz.match(/-fuzztime=30s/gmu) ?? []).length, 2);
});

test("ci-status is the stable gateway for every Go lane", () => {
  const block = jobBlock(workflowSource("ci.yml"), "ci-status");
  const expectedNeeds = [
    "markdown",
    "shellcheck",
    "shfmt",
    "typos",
    "editorconfig",
    "gitleaks",
    "lychee",
    "comment-hygiene",
    "actionlint",
    "jsonschema",
    "exec-bit",
    "machine-specific-paths",
    "eol-renormalize",
    "zizmor",
    "policy",
    "go-quality",
    "go-windows-build",
    "go-fuzz",
    "worker-image",
    "dependency-review",
  ];
  assert.deepEqual(directList(block, "needs"), expectedNeeds);

  const results = block
    .map((line) => /\$\{\{ needs\.([A-Za-z0-9_-]+)\.result \}\}/u.exec(line)?.[1])
    .filter((name) => name !== undefined);
  assert.deepEqual(results, expectedNeeds, "ci-status must aggregate every declared dependency once");
  assert.ok(block.includes("    name: ci-status"));
  assert.ok(block.includes("    if: always()"));
});
