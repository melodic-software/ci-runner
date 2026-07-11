"use strict";

const assert = require("node:assert/strict");
const test = require("node:test");

const {
  createGitHubAPI,
  reconcileRelease,
  transactionMarker,
} = require("./release-transaction.cjs");

const SOURCE_SHA = "a".repeat(40);
const OTHER_SHA = "b".repeat(40);

function releaseInput() {
  const tag = "v1.2.3";
  return {
    repository: "melodic-software/ci-runner",
    tag,
    ref: `refs/tags/${tag}`,
    sourceSHA: SOURCE_SHA,
    name: tag,
    prerelease: false,
    marker: transactionMarker(SOURCE_SHA),
    assets: [
      {name: `ci-runner-${tag}-windows-amd64.zip`, path: "/unused/archive", size: 10, digest: `sha256:${"1".repeat(64)}`},
      {name: `ci-runner-${tag}-windows-amd64.spdx.json`, path: "/unused/sbom", size: 20, digest: `sha256:${"2".repeat(64)}`},
      {name: "compatibility.json", path: "/unused/compatibility", size: 30, digest: `sha256:${"3".repeat(64)}`},
      {name: "SHA256SUMS", path: "/unused/checksums", size: 40, digest: `sha256:${"4".repeat(64)}`},
    ],
  };
}

function clone(value) {
  return structuredClone(value);
}

class FakeReleaseAPI {
  constructor(options = {}) {
    this.releases = clone(options.releases || []);
    this.tagSequence = [...(options.tagSequence || [SOURCE_SHA])];
    this.lastTag = this.tagSequence.at(-1);
    this.failCreateAfterMutation = options.failCreateAfterMutation || false;
    this.failUploadAfterMutation = options.failUploadAfterMutation || "";
    this.failPublishAfterMutation = options.failPublishAfterMutation || false;
    this.failureObserved = new Set();
    this.calls = [];
    this.nextReleaseID = 100;
    this.nextAssetID = 1000;
  }

  async resolveTagCommit() {
    this.calls.push("resolve-tag");
    if (this.tagSequence.length > 0) {
      this.lastTag = this.tagSequence.shift();
    }
    return this.lastTag;
  }

  async listReleases() {
    this.calls.push("list-releases");
    return clone(this.releases);
  }

  async getRelease(id) {
    this.calls.push(`get-release:${id}`);
    return clone(this.releases.find((release) => release.id === id));
  }

  async createDraft(input) {
    this.calls.push("create-draft");
    const release = {
      id: this.nextReleaseID++,
      tag_name: input.tag,
      name: input.name,
      body: `${input.marker}\ngenerated notes`,
      prerelease: input.prerelease,
      draft: true,
      immutable: false,
      assets: [],
    };
    this.releases.push(release);
    if (this.failCreateAfterMutation && !this.failureObserved.has("create")) {
      this.failureObserved.add("create");
      throw new Error("simulated lost create response");
    }
    return clone(release);
  }

  async deleteAsset(id) {
    this.calls.push(`delete-asset:${id}`);
    for (const release of this.releases) {
      release.assets = release.assets.filter((asset) => asset.id !== id);
    }
  }

  async uploadAsset(releaseID, expected) {
    this.calls.push(`upload-asset:${expected.name}`);
    const release = this.releases.find((candidate) => candidate.id === releaseID);
    const asset = {
      id: this.nextAssetID++,
      name: expected.name,
      state: "uploaded",
      digest: expected.digest,
      size: expected.size,
    };
    release.assets.push(asset);
    if (this.failUploadAfterMutation === expected.name && !this.failureObserved.has("upload")) {
      this.failureObserved.add("upload");
      throw new Error("simulated lost upload response");
    }
    return clone(asset);
  }

  async publishDraft(id) {
    this.calls.push(`publish-draft:${id}`);
    const release = this.releases.find((candidate) => candidate.id === id);
    release.draft = false;
    if (this.failPublishAfterMutation && !this.failureObserved.has("publish")) {
      this.failureObserved.add("publish");
      throw new Error("simulated lost publish response");
    }
    return clone(release);
  }
}

function ownedDraft(input, assets = []) {
  return {
    id: 7,
    tag_name: input.tag,
    name: input.name,
    body: `${input.marker}\nnotes`,
    prerelease: input.prerelease,
    draft: true,
    immutable: false,
    assets: clone(assets),
  };
}

function ownedPublishedRelease(input) {
  const release = ownedDraft(
    input,
    input.assets.map((asset, index) => ({
      ...clone(asset),
      id: index + 1,
      state: "uploaded",
    })),
  );
  release.draft = false;
  release.immutable = true;
  return release;
}

function response(body, status = 200) {
  return new Response(body === null ? null : JSON.stringify(body), {
    status,
    headers: {"Content-Type": "application/json"},
  });
}

test("publishes a new exact four-asset release", async () => {
  const input = releaseInput();
  const api = new FakeReleaseAPI();

  const result = await reconcileRelease(input, api);

  assert.equal(result.state, "published");
  assert.equal(api.releases.length, 1);
  assert.equal(api.releases[0].draft, false);
  assert.deepEqual(
    api.releases[0].assets.map((asset) => asset.name).sort(),
    input.assets.map((asset) => asset.name).sort(),
  );
});

test("recovers when the draft creation response is lost", async () => {
  const api = new FakeReleaseAPI({failCreateAfterMutation: true});

  const result = await reconcileRelease(releaseInput(), api);

  assert.equal(result.state, "published");
  assert.equal(api.releases.length, 1);
  assert.equal(api.calls.filter((call) => call === "create-draft").length, 1);
});

test("recovers when an uploaded asset response is lost", async () => {
  const input = releaseInput();
  const failedName = input.assets[1].name;
  const api = new FakeReleaseAPI({failUploadAfterMutation: failedName});

  const result = await reconcileRelease(input, api);

  assert.equal(result.state, "published");
  assert.equal(api.calls.filter((call) => call === `upload-asset:${failedName}`).length, 1);
  assert.equal(api.releases[0].assets.filter((asset) => asset.name === failedName).length, 1);
});

test("recovers when publication succeeds but its response is lost", async () => {
  const api = new FakeReleaseAPI({failPublishAfterMutation: true});

  const result = await reconcileRelease(releaseInput(), api);

  assert.equal(result.state, "published");
  assert.equal(api.releases.length, 1);
  assert.equal(api.releases[0].draft, false);
});

test("replaces only incomplete or unexpected assets in an owned draft", async () => {
  const input = releaseInput();
  const exact = input.assets[0];
  const api = new FakeReleaseAPI({
    releases: [
      ownedDraft(input, [
        {id: 1, name: exact.name, state: "uploaded", digest: exact.digest, size: exact.size},
        {id: 2, name: input.assets[1].name, state: "uploaded", digest: `sha256:${"9".repeat(64)}`, size: 1},
        {id: 3, name: "unexpected.txt", state: "uploaded", digest: `sha256:${"8".repeat(64)}`, size: 1},
      ]),
    ],
  });

  await reconcileRelease(input, api);

  assert(!api.calls.includes("delete-asset:1"));
  assert(api.calls.includes("delete-asset:2"));
  assert(api.calls.includes("delete-asset:3"));
  assert.equal(api.releases[0].assets.length, 4);
});

test("refuses to mutate a same-tag draft without the exact source marker", async () => {
  const input = releaseInput();
  const draft = ownedDraft(input);
  draft.body = `${transactionMarker(OTHER_SHA)}\nnotes`;
  const api = new FakeReleaseAPI({releases: [draft]});

  await assert.rejects(() => reconcileRelease(input, api), /not owned by this source transaction/);
  assert.equal(api.calls.some((call) => call.startsWith("delete-asset:") || call.startsWith("upload-asset:")), false);
});

test("fails closed if the remote tag moves before publication", async () => {
  const input = releaseInput();
  const api = new FakeReleaseAPI({tagSequence: [SOURCE_SHA, OTHER_SHA]});

  await assert.rejects(() => reconcileRelease(input, api), /remote tag resolves/);
  assert.equal(api.calls.some((call) => call.startsWith("publish-draft:")), false);
  assert.equal(api.releases[0].draft, true);
});

test("fails closed if the remote tag moves immediately after publication", async () => {
  const input = releaseInput();
  const api = new FakeReleaseAPI({tagSequence: [SOURCE_SHA, SOURCE_SHA, OTHER_SHA]});

  await assert.rejects(() => reconcileRelease(input, api), /remote tag resolves/);
  assert.equal(api.calls.filter((call) => call === "resolve-tag").length, 3);
  assert.equal(api.calls.filter((call) => call.startsWith("publish-draft:")).length, 1);
  assert.equal(api.releases[0].draft, false);
});

test("rechecks the remote tag while proving an existing published release", async () => {
  const input = releaseInput();
  const api = new FakeReleaseAPI({
    releases: [ownedPublishedRelease(input)],
    tagSequence: [SOURCE_SHA, OTHER_SHA],
  });

  await assert.rejects(() => reconcileRelease(input, api), /remote tag resolves/);
  assert.equal(api.calls.filter((call) => call === "resolve-tag").length, 2);
  assert.equal(api.calls.some((call) => call.startsWith("publish-draft:")), false);
  assert.equal(api.calls.some((call) => call.startsWith("delete-asset:") || call.startsWith("upload-asset:")), false);
});

test("fails closed on ambiguous same-tag releases", async () => {
  const input = releaseInput();
  const api = new FakeReleaseAPI({releases: [ownedDraft(input), {...ownedDraft(input), id: 8}]});

  await assert.rejects(() => reconcileRelease(input, api), /more than one release/);
  assert.equal(api.calls.includes("create-draft"), false);
});

test("peels nested annotated tags through exact Git object endpoints", async () => {
  const calls = [];
  const fetchImpl = async (url, options) => {
    calls.push({url, authorization: options.headers.Authorization});
    if (url.endsWith("/git/ref/tags/v1.2.3")) {
      return response({object: {type: "tag", sha: "1".repeat(40)}});
    }
    if (url.endsWith(`/git/tags/${"1".repeat(40)}`)) {
      return response({object: {type: "tag", sha: "2".repeat(40)}});
    }
    if (url.endsWith(`/git/tags/${"2".repeat(40)}`)) {
      return response({object: {type: "commit", sha: SOURCE_SHA}});
    }
    return response({message: "not found"}, 404);
  };
  const api = createGitHubAPI({
    token: "test-token",
    repository: "melodic-software/ci-runner",
    fetchImpl,
  });

  assert.equal(await api.resolveTagCommit("v1.2.3"), SOURCE_SHA);
  assert.equal(calls.length, 3);
  assert(calls.every((call) => call.authorization === "Bearer test-token"));
});
