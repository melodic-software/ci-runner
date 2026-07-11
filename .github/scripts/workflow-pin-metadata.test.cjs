const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const workflowDirectory = path.resolve(__dirname, "..", "workflows");
const ciWorkflowsReference = "melodic-software/ci-workflows/";
const canonicalReference =
  /^\s*uses:\s+melodic-software\/ci-workflows\/[^\s@#]+@(?<sha>[0-9a-f]{40})\s+#\s+(?<shortSha>[0-9a-f]{7})\s+(?<date>\d{4}-\d{2}-\d{2})\s*$/;

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

test("ci-workflows references use a full SHA with truthful audit metadata", () => {
  const references = [];

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
      assert.equal(match.groups.shortSha, match.groups.sha.slice(0, 7));

      const parsedDate = new Date(`${match.groups.date}T00:00:00.000Z`);
      assert.equal(
        Number.isNaN(parsedDate.valueOf())
          ? null
          : parsedDate.toISOString().slice(0, 10),
        match.groups.date,
        `${path.relative(workflowDirectory, file)}:${index + 1} has an invalid audit date`,
      );
      references.push(match.groups.sha);
    }
  }

  assert.ok(references.length > 0, "no ci-workflows references were found");
  assert.equal(
    new Set(references).size,
    1,
    "ci-workflows references must move as one reviewed compatibility pin",
  );
});
