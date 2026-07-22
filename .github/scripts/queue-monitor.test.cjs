'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  boundBodyLength,
  findOpenIncident,
  incidentMarker,
  incidentTitle,
  inspectQueuedJobs,
  MAX_BODY_LENGTH,
  MAX_STUCK_TABLE_ROWS,
  nonterminalRunStatuses,
  renderStuckMarkdownTable,
  routingRecoverySummary,
  run,
  splitList,
  upsertIncident,
} = require('./queue-monitor.cjs');

const ISSUE_AUTHOR_LOGIN = 'github-actions[bot]';
const ownIssue = (overrides) => ({ user: { login: ISSUE_AUTHOR_LOGIN, type: 'Bot' }, pull_request: undefined, ...overrides });

function fakeGitHub({ runs = {}, jobs = {}, failure } = {}) {
  const calls = [];
  const listWorkflowRunsForRepo = Symbol('runs');
  const listJobsForWorkflowRun = Symbol('jobs');
  return {
    calls,
    rest: { actions: { listWorkflowRunsForRepo, listJobsForWorkflowRun } },
    async paginate(endpoint, parameters) {
      calls.push({ endpoint, parameters });
      if (failure && failure(endpoint, parameters)) throw new Error('API page failed');
      if (endpoint === listWorkflowRunsForRepo) return runs[parameters.status] || [];
      if (endpoint === listJobsForWorkflowRun) return jobs[parameters.run_id] || [];
      throw new Error('unexpected endpoint');
    },
  };
}

test('splitList accepts comma and newline separated configuration', () => {
  assert.deepEqual(splitList('medley, standards\nci-runner'), ['medley', 'standards', 'ci-runner']);
});

test('recovery requires a verified hosted-only cutoff and fresh selector evaluation', () => {
  assert.match(routingRecoverySummary, /audited CI routing-control procedure/);
  assert.match(routingRecoverySummary, /effective `CI_RUNNER_POLICY` value `hosted-only`/);
  assert.match(routingRecoverySummary, /Re-run all jobs/);
  assert.match(routingRecoverySummary, /guarantee that the selector executes again/);
  assert.match(routingRecoverySummary, /partial-rerun dependency behavior/);
  assert.match(routingRecoverySummary, /does not recover the original pull-request check/);
  assert.doesNotMatch(routingRecoverySummary, /retry(?:ing)? (?:the )?workload/i);
});

test('queries every GitHub nonterminal run status and deduplicates runs', async () => {
  const run = { id: 42, name: 'CI', html_url: 'https://example.test/run/42' };
  const github = fakeGitHub({
    runs: Object.fromEntries(nonterminalRunStatuses.map(status => [status, [run]])),
    jobs: { 42: [] },
  });

  const stuck = await inspectQueuedJobs({
    github,
    owner: 'melodic-software',
    repositories: ['medley'],
    managedLabels: new Set(['melodic-ubuntu-24.04-x64']),
    thresholdMinutes: 5,
    now: Date.parse('2026-07-10T12:00:00Z'),
  });

  assert.deepEqual(stuck, []);
  assert.deepEqual(
    github.calls.filter(call => call.endpoint === github.rest.actions.listWorkflowRunsForRepo).map(call => call.parameters.status),
    nonterminalRunStatuses,
  );
  assert.equal(github.calls.filter(call => call.endpoint === github.rest.actions.listJobsForWorkflowRun).length, 1);
});

test('alerts only runner-eligible queued jobs with an exact managed label', async () => {
  const now = Date.parse('2026-07-10T12:00:00Z');
  const old = '2026-07-10T11:50:00Z';
  const github = fakeGitHub({
    runs: { in_progress: [{ id: 7, name: 'CI', html_url: 'https://example.test/run/7' }] },
    jobs: {
      7: [
        { name: 'eligible', status: 'queued', created_at: old, labels: ['melodic-ubuntu-24.04-x64'], html_url: 'https://example.test/job/1' },
        { name: 'needs-chain', status: 'waiting', created_at: old, labels: ['melodic-ubuntu-24.04-x64'] },
        { name: 'concurrency', status: 'pending', created_at: old, labels: ['melodic-ubuntu-24.04-x64'] },
        { name: 'not-requested', status: 'requested', created_at: old, labels: ['melodic-ubuntu-24.04-x64'] },
        { name: 'hosted', status: 'queued', created_at: old, labels: ['ubuntu-24.04'] },
        { name: 'similar-label', status: 'queued', created_at: old, labels: ['melodic-ubuntu-24.04-x64-extra'] },
      ],
    },
  });

  const stuck = await inspectQueuedJobs({
    github,
    owner: 'melodic-software',
    repositories: ['medley'],
    managedLabels: new Set(['melodic-ubuntu-24.04-x64']),
    thresholdMinutes: 5,
    now,
  });

  assert.equal(stuck.length, 1);
  assert.equal(stuck[0].job, 'eligible');
  assert.equal(stuck[0].queuedMinutes, 10);
});

test('does not alert a newly queued managed job', async () => {
  const now = Date.parse('2026-07-10T12:00:00Z');
  const github = fakeGitHub({
    runs: { queued: [{ id: 8, name: 'CI' }] },
    jobs: { 8: [{ name: 'new', status: 'queued', created_at: '2026-07-10T11:58:00Z', labels: ['managed'] }] },
  });
  const stuck = await inspectQueuedJobs({
    github,
    owner: 'owner',
    repositories: ['repo'],
    managedLabels: new Set(['managed']),
    thresholdMinutes: 5,
    now,
  });
  assert.deepEqual(stuck, []);
});

test('propagates a pagination failure so the monitor becomes visibly red', async () => {
  const github = fakeGitHub({ failure: (_endpoint, parameters) => parameters.status === 'waiting' });
  await assert.rejects(
    inspectQueuedJobs({
      github,
      owner: 'owner',
      repositories: ['repo'],
      managedLabels: new Set(['managed']),
      thresholdMinutes: 5,
    }),
    /API page failed/,
  );
});

function fakeCore() {
  const calls = { setOutput: [], setFailed: [], info: [] };
  const summaryCalls = [];
  const summary = {
    addHeading(text) { summaryCalls.push(['addHeading', text]); return summary; },
    addRaw(text) { summaryCalls.push(['addRaw', text]); return summary; },
    addTable(rows) { summaryCalls.push(['addTable', rows]); return summary; },
    async write() { summaryCalls.push(['write']); },
  };
  return {
    calls,
    summaryCalls,
    summary,
    setOutput(name, value) { calls.setOutput.push([name, value]); },
    setFailed(message) { calls.setFailed.push(message); },
    info(message) { calls.info.push(message); },
  };
}

function fakeGithubIssues({ existingIssues = [] } = {}) {
  const calls = [];
  const listForRepo = Symbol('listForRepo');
  return {
    calls,
    rest: {
      issues: {
        listForRepo,
        async create(parameters) { calls.push(['create', parameters]); return { data: { number: 101 } }; },
        async update(parameters) { calls.push(['update', parameters]); },
        async createComment(parameters) { calls.push(['createComment', parameters]); },
      },
    },
    async paginate(endpoint, parameters) {
      calls.push(['paginate', parameters]);
      if (endpoint === listForRepo) return existingIssues;
      throw new Error('unexpected endpoint');
    },
  };
}

test('run() stays green and hands the stuck list to the incident step on detection', async () => {
  const github = fakeGitHub({
    runs: { queued: [{ id: 9, name: 'CI', html_url: 'https://example.test/run/9' }] },
    jobs: {
      9: [{ name: 'build', status: 'queued', created_at: '2026-07-22T09:48:00Z', labels: ['melodic-ubuntu-24.04-x64'], html_url: 'https://example.test/job/9' }],
    },
  });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      MONITOR_OWNER: 'melodic-software',
      MONITORED_REPOSITORIES: 'medley',
      MANAGED_LABELS: 'melodic-ubuntu-24.04-x64',
      QUEUE_THRESHOLD_MINUTES: '5',
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  assert.deepEqual(core.calls.setFailed, []);
  assert.equal(core.calls.setOutput.length, 1);
  const [name, value] = core.calls.setOutput[0];
  assert.equal(name, 'stuck');
  assert.equal(JSON.parse(value).length, 1);
});

test('run() propagates a genuine execution error via setFailed and skips the stuck output', async () => {
  const github = fakeGitHub({ failure: () => true });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      MONITOR_OWNER: 'melodic-software',
      MONITORED_REPOSITORIES: 'medley',
      MANAGED_LABELS: 'melodic-ubuntu-24.04-x64',
      QUEUE_THRESHOLD_MINUTES: '5',
    },
  });

  assert.equal(core.calls.setFailed.length, 1);
  assert.deepEqual(core.calls.setOutput, []);
});

test('incidentTitle and renderStuckMarkdownTable keep a stable, greppable shape', () => {
  assert.equal(incidentTitle('melodic-software'), '[Alert] Managed runner queue capacity — melodic-software');
  const table = renderStuckMarkdownTable([
    { repository: 'melodic-software/medley', workflow: 'CI', job: 'build', queuedMinutes: 12, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' },
  ]);
  assert.match(table, /^\| Repository \| Workflow \| Job \| Minutes \| Labels \| Link \|/);
  assert.match(table, /\| melodic-software\/medley \| CI \| build \| 12 \| melodic-ubuntu-24.04-x64 \| \[open job\]\(https:\/\/example\.test\/job\/1\) \|/);
});

test('renderStuckMarkdownTable escapes pipes so a job or workflow name cannot corrupt the table', () => {
  const table = renderStuckMarkdownTable([
    { repository: 'melodic-software/medley', workflow: 'CI | matrix', job: 'build | test', queuedMinutes: 6, labels: 'a|b', url: 'https://example.test/job/2' },
  ]);
  assert.match(table, /\| CI \\\| matrix \| build \\\| test \| 6 \| a\\\|b \|/);
});

test('renderStuckMarkdownTable neutralizes HTML comment sequences so a crafted job name cannot inject a literal <!-- ... --> into the body', () => {
  const foreignMarker = incidentMarker('owner-b');
  const table = renderStuckMarkdownTable([
    { repository: 'melodic-software/medley', workflow: 'CI', job: `evil ${foreignMarker} name`, queuedMinutes: 5, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' },
  ]);
  assert.ok(!table.includes(foreignMarker), 'the raw marker substring must not survive rendering');
  assert.ok(table.includes('&lt;!--'), 'the HTML comment opener must be neutralized to an entity');
  assert.ok(table.includes('--&gt;'), 'the HTML comment closer must be neutralized to an entity');
});

function fakeStuckList(count) {
  return Array.from({ length: count }, (_, index) => ({
    repository: 'melodic-software/medley',
    workflow: 'CI',
    job: `build-${index}`,
    queuedMinutes: 10,
    labels: 'melodic-ubuntu-24.04-x64',
    url: `https://example.test/job/${index}`,
  }));
}

test('renderStuckMarkdownTable caps rows at MAX_STUCK_TABLE_ROWS and reports the correct remainder with a run link', () => {
  const total = MAX_STUCK_TABLE_ROWS + 7;
  const table = renderStuckMarkdownTable(fakeStuckList(total), { runUrl: 'https://example.test/actions/runs/123' });
  const rowLines = table.split('\n').filter(line => line.startsWith('| melodic-software/medley'));
  assert.equal(rowLines.length, MAX_STUCK_TABLE_ROWS, 'must render at most MAX_STUCK_TABLE_ROWS data rows');
  assert.match(table, /_\.\.\.and 7 more managed job\(s\) — see the \[workflow run\]\(https:\/\/example\.test\/actions\/runs\/123\) for the full list\._/);
});

test('renderStuckMarkdownTable omits the run link when none is provided but still reports the remainder count', () => {
  const total = MAX_STUCK_TABLE_ROWS + 3;
  const table = renderStuckMarkdownTable(fakeStuckList(total));
  assert.match(table, /_\.\.\.and 3 more managed job\(s\)\._/);
  assert.doesNotMatch(table, /workflow run/);
});

test('renderStuckMarkdownTable does not add a remainder note when the stuck count is within the cap', () => {
  const table = renderStuckMarkdownTable(fakeStuckList(MAX_STUCK_TABLE_ROWS));
  assert.doesNotMatch(table, /more managed job/);
});

test('boundBodyLength leaves a body under the limit untouched, appending only the marker', () => {
  const marker = incidentMarker('melodic-software');
  const body = boundBodyLength('short body', marker);
  assert.equal(body, `short body\n\n${marker}`);
});

test('boundBodyLength truncates an oversized body while preserving the marker fully intact', () => {
  const marker = incidentMarker('melodic-software');
  const oversized = 'x'.repeat(MAX_BODY_LENGTH * 2);
  const body = boundBodyLength(oversized, marker, MAX_BODY_LENGTH);
  assert.ok(body.length <= MAX_BODY_LENGTH, `bounded body must not exceed MAX_BODY_LENGTH (was ${body.length})`);
  assert.ok(body.endsWith(marker), 'the marker must survive intact at the end of a truncated body');
  assert.match(body, /truncated to stay under GitHub's issue body limit/);
});

test('upsertIncident renders a capped, length-bounded body with the run link for an oversized stuck array', async () => {
  const github = fakeGithubIssues();
  const core = fakeCore();
  const total = MAX_STUCK_TABLE_ROWS + 12;
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'melodic-software',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      GITHUB_SERVER_URL: 'https://github.com',
      GITHUB_RUN_ID: '999999',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify(fakeStuckList(total)),
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const created = github.calls.find(([action]) => action === 'create');
  assert.ok(created, 'expected an issue create call');
  const [, parameters] = created;
  assert.ok(parameters.body.length <= MAX_BODY_LENGTH, `body must stay under MAX_BODY_LENGTH (was ${parameters.body.length})`);
  assert.match(parameters.body, /_\.\.\.and 12 more managed job\(s\) — see the \[workflow run\]\(https:\/\/github\.com\/melodic-software\/ci-runner\/actions\/runs\/999999\) for the full list\._/);
  assert.ok(parameters.body.endsWith(incidentMarker('melodic-software')), 'the marker must survive at the end of the body');
});

test('findOpenIncident matches an own-authored issue carrying the marker and ignores pull requests', async () => {
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [
      ownIssue({ number: 1, title: 'unrelated', body: 'no marker here' }),
      ownIssue({ number: 2, body: `has marker ${marker}`, pull_request: { url: 'x' } }),
      ownIssue({ number: 3, body: `has marker ${marker}` }),
    ],
  });
  const found = await findOpenIncident({ github, homeOwner: 'melodic-software', homeRepo: 'ci-runner', marker, issueAuthorLogin: ISSUE_AUTHOR_LOGIN });
  assert.equal(found.number, 3);
});

test('findOpenIncident filters to the automated label server-side', async () => {
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({ existingIssues: [] });
  await findOpenIncident({ github, homeOwner: 'melodic-software', homeRepo: 'ci-runner', marker, issueAuthorLogin: ISSUE_AUTHOR_LOGIN });
  const [, parameters] = github.calls.find(([action]) => action === 'paginate');
  assert.equal(parameters.labels, 'automated');
  assert.equal(parameters.state, 'open');
});

test('findOpenIncident rejects a decoy issue that carries the marker but was not opened by this workflow\'s own identity', async () => {
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [
      { number: 9, body: `decoy ${marker}`, pull_request: undefined, user: { login: 'kyle-sexton', type: 'User' } },
      { number: 10, body: `decoy ${marker}`, pull_request: undefined, user: { login: 'some-other-bot', type: 'Bot' } },
    ],
  });
  const found = await findOpenIncident({ github, homeOwner: 'melodic-software', homeRepo: 'ci-runner', marker, issueAuthorLogin: ISSUE_AUTHOR_LOGIN });
  assert.equal(found, null);
});

test('findOpenIncident fails closed when more than one own-authored issue carries the marker', async () => {
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [
      ownIssue({ number: 3, body: `has marker ${marker}` }),
      ownIssue({ number: 4, body: `has marker ${marker}` }),
    ],
  });
  await assert.rejects(
    findOpenIncident({ github, homeOwner: 'melodic-software', homeRepo: 'ci-runner', marker, issueAuthorLogin: ISSUE_AUTHOR_LOGIN }),
    /Found 2 open incident issues carrying marker/,
  );
});

test('a crafted job name embedding another owner\'s marker cannot cause cross-owner incident adoption', async () => {
  const foreignOwner = 'owner-b';
  const foreignMarker = incidentMarker(foreignOwner);
  const github = fakeGithubIssues();
  const core = fakeCore();

  // owner-a's detection includes a job whose name is crafted to contain
  // owner-b's marker verbatim.
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'owner-a',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify([{ repository: 'melodic-software/medley', workflow: 'CI', job: `evil ${foreignMarker} name`, queuedMinutes: 5, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' }]),
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const created = github.calls.find(([action]) => action === 'create');
  assert.ok(created, 'expected owner-a\'s incident to be created');
  const [, parameters] = created;

  // Feed owner-a's freshly created issue body back in and search for
  // owner-b's incident: the injected marker must not have survived
  // rendering, so owner-b must not adopt owner-a's issue.
  const githubForOwnerB = fakeGithubIssues({
    existingIssues: [ownIssue({ number: 999, title: parameters.title, body: parameters.body })],
  });
  const found = await findOpenIncident({
    github: githubForOwnerB,
    homeOwner: 'melodic-software',
    homeRepo: 'ci-runner',
    marker: foreignMarker,
    issueAuthorLogin: ISSUE_AUTHOR_LOGIN,
  });
  assert.equal(found, null, 'owner-b must not adopt owner-a\'s incident even though a crafted job name tried to embed owner-b\'s marker');
});

test('a job name that exactly equals a legitimate marker string still renders inert after escaping', () => {
  const marker = incidentMarker('melodic-software');
  const table = renderStuckMarkdownTable([
    { repository: 'melodic-software/medley', workflow: 'CI', job: marker, queuedMinutes: 3, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/exact' },
  ]);
  assert.ok(!table.includes(marker), 'a job name that is exactly a marker string must not survive rendering as a functional marker');
});

test('pipe-escaping and HTML-comment-escaping compose without interfering with each other', () => {
  const table = renderStuckMarkdownTable([
    { repository: 'melodic-software/medley', workflow: 'CI <!-- | --> matrix', job: 'build', queuedMinutes: 4, labels: 'x', url: 'https://example.test/job/combo' },
  ]);
  assert.match(table, /CI &lt;!-- \\\| --&gt; matrix/);
  assert.ok(!table.includes('<!--') && !table.includes('-->'), 'no raw HTML comment sequence may survive alongside an escaped pipe');
});

// End-to-end proof (per independent review, escalated CRITICAL against an
// earlier commit that lacked this escaping) that a crafted job name cannot
// achieve any of three failure modes against a DIFFERENT owner that has a
// genuine, currently open incident: adopt-and-overwrite it, false-close it,
// or trigger a false fail-closed ambiguity error that reds out that owner's
// run. Bodies below are built through the real renderStuckMarkdownTable /
// upsertIncident body-assembly shape, not hand-written, so the test exercises
// the actual escaping pipeline end to end.
function buildRealIncidentBody(owner, jobName, createdIso, nowIso) {
  return [
    `Managed runner queue capacity alert for \`${owner}\`.`,
    '',
    `Capacity window: constrained since ${createdIso} (last confirmed ${nowIso}). Affected queue depth: 1 managed job(s).`,
    '',
    renderStuckMarkdownTable([{ repository: 'melodic-software/medley', workflow: 'CI', job: jobName, queuedMinutes: 10, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/real' }]),
    '',
    routingRecoverySummary.trim(),
    '',
    incidentMarker(owner),
  ].join('\n');
}

test('findOpenIncident resolves exactly owner-b\'s real incident without ambiguity, even though owner-a\'s incident carries an injected owner-b marker', async () => {
  const ownerBMarker = incidentMarker('owner-b');
  const github = fakeGithubIssues({
    existingIssues: [
      ownIssue({ number: 100, title: incidentTitle('owner-b'), body: buildRealIncidentBody('owner-b', 'legit-build', '2026-07-22T08:00:00.000Z', '2026-07-22T08:00:00.000Z'), created_at: '2026-07-22T08:00:00.000Z' }),
      ownIssue({ number: 101, title: incidentTitle('owner-a'), body: buildRealIncidentBody('owner-a', `evil ${ownerBMarker} name`, '2026-07-22T09:00:00.000Z', '2026-07-22T09:00:00.000Z'), created_at: '2026-07-22T09:00:00.000Z' }),
    ],
  });
  const found = await findOpenIncident({ github, homeOwner: 'melodic-software', homeRepo: 'ci-runner', marker: ownerBMarker, issueAuthorLogin: ISSUE_AUTHOR_LOGIN });
  assert.equal(found.number, 100, 'must resolve to owner-b\'s own real issue, never owner-a\'s, and never throw ambiguity');
});

test('upsertIncident updates only owner-b\'s real issue while still stuck, never adopting owner-a\'s issue via an injected marker (prevents adopt-overwrite)', async () => {
  const ownerBMarker = incidentMarker('owner-b');
  const github = fakeGithubIssues({
    existingIssues: [
      ownIssue({ number: 100, title: incidentTitle('owner-b'), body: buildRealIncidentBody('owner-b', 'legit-build', '2026-07-22T08:00:00.000Z', '2026-07-22T08:00:00.000Z'), created_at: '2026-07-22T08:00:00.000Z' }),
      ownIssue({ number: 101, title: incidentTitle('owner-a'), body: buildRealIncidentBody('owner-a', `evil ${ownerBMarker} name`, '2026-07-22T09:00:00.000Z', '2026-07-22T09:00:00.000Z'), created_at: '2026-07-22T09:00:00.000Z' }),
    ],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'owner-b',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify([{ repository: 'melodic-software/medley', workflow: 'CI', job: 'still-stuck', queuedMinutes: 12, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/still' }]),
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const updateCalls = github.calls.filter(([action]) => action === 'update');
  assert.equal(updateCalls.length, 1, 'exactly one issue must be updated');
  assert.equal(updateCalls[0][1].issue_number, 100, 'the update must target owner-b\'s own real issue, never owner-a\'s');
  assert.equal(github.calls.filter(([action]) => action === 'create').length, 0, 'no duplicate incident should be created when owner-b\'s real one is correctly found');
});

test('upsertIncident closes only owner-b\'s real issue on recovery, never owner-a\'s issue via an injected marker (prevents false-close)', async () => {
  const ownerBMarker = incidentMarker('owner-b');
  const github = fakeGithubIssues({
    existingIssues: [
      ownIssue({ number: 100, title: incidentTitle('owner-b'), body: buildRealIncidentBody('owner-b', 'legit-build', '2026-07-22T08:00:00.000Z', '2026-07-22T08:00:00.000Z'), created_at: '2026-07-22T08:00:00.000Z' }),
      ownIssue({ number: 101, title: incidentTitle('owner-a'), body: buildRealIncidentBody('owner-a', `evil ${ownerBMarker} name`, '2026-07-22T09:00:00.000Z', '2026-07-22T09:00:00.000Z'), created_at: '2026-07-22T09:00:00.000Z' }),
    ],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { TARGET_OWNER: 'owner-b', GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN, STUCK_JSON: '[]' },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const updateCalls = github.calls.filter(([action]) => action === 'update');
  assert.equal(updateCalls.length, 1, 'exactly one issue must be closed');
  assert.equal(updateCalls[0][1].issue_number, 100, 'recovery must close owner-b\'s own real issue, never owner-a\'s');
  assert.equal(updateCalls[0][1].state, 'closed');
  const commentCalls = github.calls.filter(([action]) => action === 'createComment');
  assert.equal(commentCalls.length, 1);
  assert.equal(commentCalls[0][1].issue_number, 100);
});

test('incidentMarker keeps prefix-related owners from substring-colliding', () => {
  const shortOwner = incidentMarker('melodic-software');
  const longOwner = incidentMarker('melodic-software-fork');
  assert.ok(!longOwner.includes(shortOwner), 'the longer owner\'s marker must not contain the shorter owner\'s marker as a substring');
  assert.ok(!shortOwner.includes(longOwner), 'the shorter owner\'s marker must not contain the longer owner\'s marker as a substring');
});

test('upsertIncident opens a new incident issue when none is open and jobs are stuck', async () => {
  const github = fakeGithubIssues();
  const core = fakeCore();
  const now = Date.parse('2026-07-22T10:00:00Z');
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'melodic-software',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify([{ repository: 'melodic-software/medley', workflow: 'CI', job: 'build', queuedMinutes: 12, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' }]),
    },
    now,
  });

  const created = github.calls.find(([action]) => action === 'create');
  assert.ok(created, 'expected an issue create call');
  const [, parameters] = created;
  assert.equal(parameters.owner, 'melodic-software');
  assert.equal(parameters.repo, 'ci-runner');
  assert.equal(parameters.title, incidentTitle('melodic-software'));
  assert.deepEqual(parameters.labels, ['automated']);
  assert.match(parameters.body, /Affected queue depth: 1 managed job\(s\)/);
  assert.match(parameters.body, /constrained since 2026-07-22T10:00:00\.000Z/);
  assert.ok(parameters.body.includes(incidentMarker('melodic-software')), 'body must carry the incident marker');
  assert.equal(github.calls.filter(([action]) => action === 'update' || action === 'createComment').length, 0);
});

test('upsertIncident silently updates an already-open incident, preserving the window start and without commenting', async () => {
  const title = incidentTitle('melodic-software');
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [ownIssue({ number: 55, title, body: `stale ${marker}`, created_at: '2026-07-22T09:00:00.000Z' })],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'melodic-software',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify([
        { repository: 'melodic-software/medley', workflow: 'CI', job: 'build', queuedMinutes: 12, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' },
        { repository: 'melodic-software/standards', workflow: 'CI', job: 'lint', queuedMinutes: 8, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/2' },
      ]),
    },
    now: Date.parse('2026-07-22T10:15:00Z'),
  });

  const updated = github.calls.find(([action]) => action === 'update');
  assert.ok(updated, 'expected an update call');
  assert.equal(updated[1].issue_number, 55);
  assert.match(updated[1].body, /constrained since 2026-07-22T09:00:00\.000Z/);
  assert.match(updated[1].body, /Affected queue depth: 2 managed job\(s\)/);
  assert.equal(github.calls.filter(([action]) => action === 'create' || action === 'createComment').length, 0);
});

test('upsertIncident does not adopt a decoy issue: opens a fresh incident instead', async () => {
  const title = incidentTitle('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [{ number: 66, title, body: 'no marker, wrong author', pull_request: undefined, user: { login: 'kyle-sexton', type: 'User' } }],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      TARGET_OWNER: 'melodic-software',
      GITHUB_REPOSITORY: 'melodic-software/ci-runner',
      ISSUE_AUTHOR_LOGIN,
      STUCK_JSON: JSON.stringify([{ repository: 'melodic-software/medley', workflow: 'CI', job: 'build', queuedMinutes: 12, labels: 'melodic-ubuntu-24.04-x64', url: 'https://example.test/job/1' }]),
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const created = github.calls.find(([action]) => action === 'create');
  assert.ok(created, 'expected a fresh issue create call, not an update of the decoy');
  assert.equal(github.calls.filter(([action]) => action === 'update').length, 0);
});

test('upsertIncident closes and comments the incident on recovery', async () => {
  const title = incidentTitle('melodic-software');
  const marker = incidentMarker('melodic-software');
  const github = fakeGithubIssues({
    existingIssues: [ownIssue({ number: 55, title, body: `stale ${marker}`, created_at: '2026-07-22T09:00:00.000Z' })],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { TARGET_OWNER: 'melodic-software', GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN, STUCK_JSON: '[]' },
    now: Date.parse('2026-07-22T11:00:00Z'),
  });

  const commented = github.calls.find(([action]) => action === 'createComment');
  const updated = github.calls.find(([action]) => action === 'update');
  assert.ok(commented && updated, 'expected a recovery comment and a close update');
  assert.equal(commented[1].issue_number, 55);
  assert.match(commented[1].body, /2026-07-22T09:00:00\.000Z.*2026-07-22T11:00:00\.000Z/s);
  assert.equal(updated[1].issue_number, 55);
  assert.equal(updated[1].state, 'closed');
  assert.equal(updated[1].state_reason, 'completed');
});

test('upsertIncident is a no-op when recovered and no incident is open', async () => {
  const github = fakeGithubIssues();
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { TARGET_OWNER: 'melodic-software', GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN, STUCK_JSON: '[]' },
    now: Date.parse('2026-07-22T11:00:00Z'),
  });

  assert.deepEqual(github.calls.filter(([action]) => action !== 'paginate'), []);
  assert.equal(core.calls.info.length, 1);
});

test('upsertIncident rejects a missing home repository, target owner, or issue-author login', async () => {
  const github = fakeGithubIssues();
  const core = fakeCore();
  await assert.rejects(
    upsertIncident({ github, core, env: { TARGET_OWNER: 'melodic-software', ISSUE_AUTHOR_LOGIN, STUCK_JSON: '[]' } }),
    /GITHUB_REPOSITORY must be set/,
  );
  await assert.rejects(
    upsertIncident({ github, core, env: { GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN, STUCK_JSON: '[]' } }),
    /TARGET_OWNER is required/,
  );
  await assert.rejects(
    upsertIncident({ github, core, env: { TARGET_OWNER: 'melodic-software', GITHUB_REPOSITORY: 'melodic-software/ci-runner', STUCK_JSON: '[]' } }),
    /ISSUE_AUTHOR_LOGIN is required/,
  );
});

test('upsertIncident rejects a missing or empty STUCK_JSON instead of silently treating it as recovered', async () => {
  const github = fakeGithubIssues({
    existingIssues: [ownIssue({ number: 55, body: `open ${incidentMarker('melodic-software')}`, created_at: '2026-07-22T09:00:00.000Z' })],
  });
  const core = fakeCore();
  const baseEnv = { TARGET_OWNER: 'melodic-software', GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN };

  await assert.rejects(upsertIncident({ github, core, env: baseEnv }), /STUCK_JSON is required/);
  await assert.rejects(upsertIncident({ github, core, env: { ...baseEnv, STUCK_JSON: '' } }), /STUCK_JSON is required/);

  // Neither rejection may have closed the pre-existing open incident.
  assert.equal(github.calls.filter(([action]) => action === 'update' || action === 'createComment').length, 0);
});

test('upsertIncident rejects malformed or non-array STUCK_JSON', async () => {
  const github = fakeGithubIssues();
  const core = fakeCore();
  const baseEnv = { TARGET_OWNER: 'melodic-software', GITHUB_REPOSITORY: 'melodic-software/ci-runner', ISSUE_AUTHOR_LOGIN };

  await assert.rejects(upsertIncident({ github, core, env: { ...baseEnv, STUCK_JSON: '{not json' } }), /STUCK_JSON is not valid JSON/);
  await assert.rejects(upsertIncident({ github, core, env: { ...baseEnv, STUCK_JSON: '{}' } }), /STUCK_JSON must decode to an array/);
});

test('rejects incomplete configuration and invalid thresholds', async () => {
  const github = fakeGitHub();
  await assert.rejects(
    inspectQueuedJobs({ github, owner: '', repositories: [], managedLabels: new Set(), thresholdMinutes: 5 }),
    /configuration is incomplete/,
  );
  await assert.rejects(
    inspectQueuedJobs({ github, owner: 'owner', repositories: ['repo'], managedLabels: new Set(['managed']), thresholdMinutes: 0 }),
    /threshold must be a positive/,
  );
});
