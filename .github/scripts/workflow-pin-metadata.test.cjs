const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const workflowDirectory = path.resolve(__dirname, "..", "workflows");
const ciWorkflowsReference = "melodic-software/ci-workflows/";
const canonicalReference =
  /^\s*uses:\s+melodic-software\/ci-workflows\/[^\s@#]+@(?<sha>[0-9a-f]{40})\s+#\s+(?<version>v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))\s*$/;

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

test("ci-workflows references use a full SHA with one release version", () => {
  const references = [];
  const versions = [];

  for (const file of workflowFiles(workflowDirectory)) {
    const lines = fs.readFileSync(file, "utf8").split(/\r?\n/u);
    for (const [index, line] of lines.entries()) {
      if (!line.includes(ciWorkflowsReference)) {
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

  assert.ok(references.length > 0, "no ci-workflows references were found");
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
});
