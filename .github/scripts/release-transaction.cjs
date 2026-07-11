"use strict";

const {createHash} = require("node:crypto");
const {lstat, readFile, realpath} = require("node:fs/promises");
const path = require("node:path");

const API_VERSION = "2026-03-10";
const MAX_TAG_DEPTH = 10;
const RELEASE_PATTERN = /^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*))*)?$/;
const SHA_PATTERN = /^[0-9a-f]{40}$/;
const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;
const REPOSITORY_PATTERN = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

class GitHubAPIError extends Error {
  constructor(message, status) {
    super(message);
    this.name = "GitHubAPIError";
    this.status = status;
  }
}

function invariant(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

function transactionMarker(sourceSHA) {
  return `<!-- ci-runner-release-transaction:v1:${sourceSHA} -->`;
}

function validateInput(input) {
  invariant(REPOSITORY_PATTERN.test(input.repository), "release repository is invalid");
  invariant(RELEASE_PATTERN.test(input.tag), "release tag is not strict supported SemVer");
  invariant(SHA_PATTERN.test(input.sourceSHA), "release source SHA is invalid");
  invariant(input.ref === `refs/tags/${input.tag}`, "release ref does not match the requested tag");
  invariant(input.name === input.tag, "release name must equal its tag");
  invariant(input.marker === transactionMarker(input.sourceSHA), "release transaction marker is invalid");
  invariant(Array.isArray(input.assets) && input.assets.length === 4, "release requires exactly four assets");

  const names = new Set();
  for (const asset of input.assets) {
    invariant(typeof asset.name === "string" && path.basename(asset.name) === asset.name, "release asset name is unsafe");
    invariant(!names.has(asset.name), `duplicate release asset name: ${asset.name}`);
    invariant(DIGEST_PATTERN.test(asset.digest), `release asset digest is invalid: ${asset.name}`);
    invariant(Number.isSafeInteger(asset.size) && asset.size >= 0, `release asset size is invalid: ${asset.name}`);
    names.add(asset.name);
  }
}

function assertReleaseIdentity(release, input) {
  invariant(release !== null && typeof release === "object", "release response is invalid");
  invariant(Number.isSafeInteger(release.id) && release.id > 0, "release ID is invalid");
  invariant(release.tag_name === input.tag, "same-tag release has an unexpected tag");
  invariant(release.name === input.name, "same-tag release has an unexpected name");
  invariant(release.prerelease === input.prerelease, "same-tag release has an unexpected prerelease state");
  invariant(typeof release.body === "string" && release.body.startsWith(input.marker), "same-tag draft is not owned by this source transaction");
  invariant(Array.isArray(release.assets), "same-tag release has an invalid asset collection");
}

function assetMatches(actual, expected) {
  return (
    actual !== null &&
    typeof actual === "object" &&
    actual.name === expected.name &&
    actual.state === "uploaded" &&
    actual.digest === expected.digest &&
    actual.size === expected.size
  );
}

function assertExactAssets(release, expectedAssets) {
  invariant(release.assets.length === expectedAssets.length, "release asset set is not exact");
  const expectedByName = new Map(expectedAssets.map((asset) => [asset.name, asset]));
  const seen = new Set();
  for (const actual of release.assets) {
    invariant(typeof actual.name === "string" && !seen.has(actual.name), "release asset names are invalid or duplicated");
    const expected = expectedByName.get(actual.name);
    invariant(expected !== undefined, `release contains an unexpected asset: ${actual.name}`);
    invariant(assetMatches(actual, expected), `release asset does not match local evidence: ${actual.name}`);
    seen.add(actual.name);
  }
}

async function matchingRelease(api, input) {
  const matches = (await api.listReleases()).filter((release) => release.tag_name === input.tag);
  invariant(matches.length <= 1, "more than one release exists for the requested tag");
  return matches[0];
}

async function assertRemoteTag(api, input) {
  const observed = await api.resolveTagCommit(input.tag);
  invariant(observed === input.sourceSHA, `remote tag resolves to ${observed}, expected ${input.sourceSHA}`);
}

async function recoverCreatedDraft(api, input, originalError) {
  const recovered = await matchingRelease(api, input);
  if (!recovered) {
    throw originalError;
  }
  assertReleaseIdentity(recovered, input);
  invariant(recovered.draft === true, "release creation failed but the recovered same-tag release is not a draft");
  return recovered;
}

async function reconcileDraftAssets(api, release, input) {
  let current = await api.getRelease(release.id);
  assertReleaseIdentity(current, input);
  invariant(current.draft === true, "release became published before draft reconciliation completed");

  const expectedByName = new Map(input.assets.map((asset) => [asset.name, asset]));
  for (const actual of current.assets) {
    const expected = expectedByName.get(actual.name);
    if (expected !== undefined && assetMatches(actual, expected)) {
      continue;
    }
    invariant(Number.isSafeInteger(actual.id) && actual.id > 0, "replaceable draft asset has no valid ID");
    await api.deleteAsset(actual.id);
  }

  current = await api.getRelease(release.id);
  for (const expected of input.assets) {
    const existing = current.assets.find((asset) => asset.name === expected.name);
    if (assetMatches(existing, expected)) {
      continue;
    }

    try {
      await api.uploadAsset(release.id, expected);
    } catch (error) {
      // The upload request can succeed server-side while the runner loses its
      // response. Re-read before deciding whether a retry is safe.
      current = await api.getRelease(release.id);
      const recovered = current.assets.find((asset) => asset.name === expected.name);
      if (!assetMatches(recovered, expected)) {
        throw error;
      }
    }
    current = await api.getRelease(release.id);
  }

  assertReleaseIdentity(current, input);
  invariant(current.draft === true, "release became published while assets were uploading");
  assertExactAssets(current, input.assets);
  return current;
}

async function reconcileRelease(input, api) {
  validateInput(input);
  await assertRemoteTag(api, input);

  let release = await matchingRelease(api, input);
  if (release && release.draft === false) {
    assertReleaseIdentity(release, input);
    assertExactAssets(release, input.assets);
    await assertRemoteTag(api, input);
    return {id: release.id, state: "published"};
  }

  if (!release) {
    try {
      release = await api.createDraft(input);
    } catch (error) {
      release = await recoverCreatedDraft(api, input, error);
    }
  }

  assertReleaseIdentity(release, input);
  invariant(release.draft === true, "same-tag release is neither an owned draft nor a verified published release");
  release = await reconcileDraftAssets(api, release, input);

  // The tag is mutable until the draft becomes an immutable published release.
  // Bind it again immediately before the one-way publication transition.
  await assertRemoteTag(api, input);
  try {
    release = await api.publishDraft(release.id, input);
  } catch (error) {
    const recovered = await matchingRelease(api, input);
    if (!recovered || recovered.draft !== false) {
      throw error;
    }
    release = recovered;
  }

  assertReleaseIdentity(release, input);
  invariant(release.draft === false, "release publication did not leave draft state");
  assertExactAssets(release, input.assets);
  await assertRemoteTag(api, input);
  return {id: release.id, state: "published"};
}

function createGitHubAPI(options) {
  const [owner, repository] = options.repository.split("/");
  const apiURL = (options.apiURL || "https://api.github.com").replace(/\/$/, "");
  const uploadsURL = (options.uploadsURL || "https://uploads.github.com").replace(/\/$/, "");
  const fetchImpl = options.fetchImpl || globalThis.fetch;

  async function request(url, requestOptions = {}) {
    const response = await fetchImpl(url.startsWith("https://") ? url : `${apiURL}${url}`, {
      ...requestOptions,
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${options.token}`,
        "User-Agent": "melodic-software-ci-runner-release",
        "X-GitHub-Api-Version": API_VERSION,
        ...requestOptions.headers,
      },
      signal: AbortSignal.timeout(30_000),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new GitHubAPIError(`GitHub API request failed (${response.status} ${response.statusText})`, response.status);
    }
    if (text.length === 0) {
      return null;
    }
    try {
      return JSON.parse(text);
    } catch {
      throw new GitHubAPIError("GitHub API returned malformed JSON", response.status);
    }
  }

  async function listReleases() {
    const releases = [];
    for (let page = 1; ; page += 1) {
      const batch = await request(`/repos/${owner}/${repository}/releases?per_page=100&page=${page}`);
      invariant(Array.isArray(batch), "GitHub release list is malformed");
      releases.push(...batch);
      if (batch.length < 100) {
        return releases;
      }
    }
  }

  async function resolveTagCommit(tag) {
    const encodedTag = encodeURIComponent(tag);
    const reference = await request(`/repos/${owner}/${repository}/git/ref/tags/${encodedTag}`);
    let object = reference?.object;
    for (let depth = 0; depth < MAX_TAG_DEPTH; depth += 1) {
      invariant(object && SHA_PATTERN.test(object.sha), "remote tag contains an invalid Git object");
      if (object.type === "commit") {
        return object.sha;
      }
      invariant(object.type === "tag", `remote tag has unsupported object type: ${object.type}`);
      const tagObject = await request(`/repos/${owner}/${repository}/git/tags/${object.sha}`);
      object = tagObject?.object;
    }
    throw new Error("remote tag nesting exceeds the supported depth");
  }

  return {
    listReleases,
    resolveTagCommit,
    getRelease: (id) => request(`/repos/${owner}/${repository}/releases/${id}`),
    createDraft: (input) =>
      request(`/repos/${owner}/${repository}/releases`, {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({
          tag_name: input.tag,
          target_commitish: input.sourceSHA,
          name: input.name,
          body: `${input.marker}\n`,
          draft: true,
          prerelease: input.prerelease,
          generate_release_notes: true,
        }),
      }),
    deleteAsset: (id) => request(`/repos/${owner}/${repository}/releases/assets/${id}`, {method: "DELETE"}),
    uploadAsset: async (releaseID, asset) => {
      const contents = await readFile(asset.path);
      return request(
        `${uploadsURL}/repos/${owner}/${repository}/releases/${releaseID}/assets?name=${encodeURIComponent(asset.name)}`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/octet-stream",
            "Content-Length": String(contents.length),
          },
          body: contents,
        },
      );
    },
    publishDraft: (id, input) =>
      request(`/repos/${owner}/${repository}/releases/${id}`, {
        method: "PATCH",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({
          tag_name: input.tag,
          target_commitish: input.sourceSHA,
          name: input.name,
          draft: false,
          prerelease: input.prerelease,
          make_latest: input.prerelease ? "false" : "legacy",
        }),
      }),
  };
}

async function loadAssets(paths, expectedNames) {
  invariant(paths.length === expectedNames.length, "release command received the wrong number of asset paths");
  const assets = [];
  for (let index = 0; index < paths.length; index += 1) {
    const requestedPath = path.resolve(paths[index]);
    const metadata = await lstat(requestedPath);
    invariant(metadata.isFile() && !metadata.isSymbolicLink(), `release asset is not a regular file: ${paths[index]}`);
    const absolutePath = await realpath(requestedPath);
    invariant(path.basename(absolutePath) === expectedNames[index], `release asset has an unexpected name: ${paths[index]}`);
    const contents = await readFile(absolutePath);
    assets.push({
      name: expectedNames[index],
      path: absolutePath,
      size: contents.length,
      digest: `sha256:${createHash("sha256").update(contents).digest("hex")}`,
    });
  }
  return assets;
}

function inputFromEnvironment(assets = []) {
  const tag = process.env.RELEASE_VERSION || "";
  const sourceSHA = process.env.GITHUB_SHA || "";
  return {
    repository: process.env.GITHUB_REPOSITORY || "",
    tag,
    ref: process.env.GITHUB_REF || "",
    sourceSHA,
    name: tag,
    prerelease: tag.includes("-"),
    marker: transactionMarker(sourceSHA),
    assets,
  };
}

async function main(argv) {
  const command = argv[0];
  invariant(process.env.GH_TOKEN, "GH_TOKEN is required");
  const api = createGitHubAPI({
    token: process.env.GH_TOKEN,
    repository: process.env.GITHUB_REPOSITORY || "",
    apiURL: process.env.GITHUB_API_URL,
  });

  if (command === "verify-tag") {
    const input = inputFromEnvironment();
    invariant(REPOSITORY_PATTERN.test(input.repository), "release repository is invalid");
    invariant(RELEASE_PATTERN.test(input.tag), "release tag is invalid");
    invariant(SHA_PATTERN.test(input.sourceSHA), "release source SHA is invalid");
    invariant(input.ref === `refs/tags/${input.tag}`, "release ref does not match the requested tag");
    await assertRemoteTag(api, input);
    process.stdout.write(`${JSON.stringify({tag: input.tag, sourceSHA: input.sourceSHA, valid: true})}\n`);
    return;
  }

  invariant(command === "reconcile", "usage: release-transaction.cjs verify-tag | reconcile ASSET... ");
  const expectedNames = [
    `ci-runner-${process.env.RELEASE_VERSION}-windows-amd64.zip`,
    `ci-runner-${process.env.RELEASE_VERSION}-windows-amd64.spdx.json`,
    "compatibility.json",
    "SHA256SUMS",
  ];
  const assets = await loadAssets(argv.slice(1), expectedNames);
  const input = inputFromEnvironment(assets);
  const result = await reconcileRelease(input, api);
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

module.exports = Object.freeze({
  GitHubAPIError,
  assertExactAssets,
  assertRemoteTag,
  assetMatches,
  createGitHubAPI,
  reconcileRelease,
  transactionMarker,
  validateInput,
});

if (require.main === module) {
  main(process.argv.slice(2)).catch((error) => {
    process.stderr.write(`release transaction failed: ${error.message}\n`);
    process.exitCode = 1;
  });
}
