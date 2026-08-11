'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  billingMonth,
  formatBillingMonth,
  hasRequiredFields,
  isStandardHostedSku,
  MAX_SKU_TABLE_ROWS,
  minutesForRow,
  parseBudget,
  POOL_ALERT_THRESHOLD_PERCENTS,
  poolIncidentMarker,
  poolIncidentTitle,
  poolRecoverySummary,
  renderSkuMarkdownTable,
  resolveRepositoryVisibility,
  run,
  STANDARD_HOSTED_SKUS,
  summarizeUsage,
  tierMarker,
  upsertIncident,
} = require('./budget-monitor.cjs');
const { incidentMarker: queueIncidentMarker } = require('./queue-monitor.cjs');

const ISSUE_AUTHOR_LOGIN = 'github-actions[bot]';
const ownIssue = (overrides) => ({ user: { login: ISSUE_AUTHOR_LOGIN, type: 'Bot' }, pull_request: undefined, ...overrides });

function usageItem(overrides = {}) {
  return {
    date: '2026-07-15',
    product: 'actions',
    sku: 'Actions Linux',
    quantity: 100,
    unitType: 'minutes',
    pricePerUnit: 0.008,
    grossAmount: 0.8,
    discountAmount: 0.8,
    netAmount: 0,
    organizationName: 'melodic-software',
    repositoryName: 'medley',
    ...overrides,
  };
}

function fakeCore() {
  const calls = { setOutput: [], setFailed: [], info: [], warning: [] };
  const summary = {
    addHeading() { return summary; },
    addRaw() { return summary; },
    addTable() { return summary; },
    async write() {},
  };
  return {
    calls,
    summary,
    setOutput(name, value) { calls.setOutput.push([name, value]); },
    setFailed(message) { calls.setFailed.push(message); },
    warning(message) { calls.warning.push(message); },
    info(message) { calls.info.push(message); },
  };
}

function fakeGitHub({
  usageItems = [],
  requestFailure,
  existingIssues = [],
  repoVisibility = {},
  repoLookupFailure = {},
} = {}) {
  const calls = [];
  const listForRepo = Symbol('listForRepo');
  const getRepo = Symbol('getRepo');
  return {
    calls,
    rest: {
      repos: {
        get: getRepo,
        async get(parameters) {
          calls.push(['repos.get', parameters]);
          const key = `${parameters.owner}/${parameters.repo}`;
          if (repoLookupFailure[key]) throw new Error('repo lookup failed');
          const privateRepo = repoVisibility[key] ?? true;
          return { data: { private: privateRepo } };
        },
      },
      issues: {
        listForRepo,
        async create(parameters) { calls.push(['create', parameters]); return { data: { number: 101 } }; },
        async update(parameters) { calls.push(['update', parameters]); },
        async createComment(parameters) { calls.push(['createComment', parameters]); },
      },
    },
    async request(route, parameters) {
      calls.push(['request', route, parameters]);
      if (requestFailure) throw new Error('billing usage request failed');
      return { data: { usageItems } };
    },
    async paginate(endpoint, parameters) {
      calls.push(['paginate', parameters]);
      if (endpoint === listForRepo) return existingIssues;
      throw new Error('unexpected endpoint');
    },
  };
}

const baseUpsertEnv = {
  GITHUB_REPOSITORY: 'melodic-software/ci-runner',
  ISSUE_AUTHOR_LOGIN,
  BUDGET_MONITOR_ARMED: 'true',
};

function budgetFixture(overrides = {}) {
  return {
    org: 'melodic-software',
    month: '2026-07',
    includedMinutes: 3000,
    consumedMinutes: 1500,
    percentUsed: 50,
    breachedThresholds: [50],
    perSku: [{ sku: 'Actions Linux', minutes: 1500 }],
    ...overrides,
  };
}

test('isStandardHostedSku accepts only the three standard hosted runner SKUs', () => {
  for (const sku of STANDARD_HOSTED_SKUS) {
    assert.ok(isStandardHostedSku(usageItem({ sku })));
  }
  assert.ok(!isStandardHostedSku(usageItem({ sku: 'Actions Linux 16-core' })));
  assert.ok(!isStandardHostedSku(usageItem({ sku: 'Self-hosted' })));
});

test('hasRequiredFields rejects rows missing organization, repository, sku, or quantity', () => {
  assert.ok(hasRequiredFields(usageItem()));
  assert.ok(!hasRequiredFields(usageItem({ organizationName: '' })));
  assert.ok(!hasRequiredFields(usageItem({ repositoryName: '' })));
  assert.ok(!hasRequiredFields(usageItem({ sku: '' })));
  assert.ok(!hasRequiredFields(usageItem({ quantity: undefined })));
  assert.ok(!hasRequiredFields(usageItem({ quantity: -1 })));
});

test('minutesForRow uses raw quantity for private and unknown visibility, zero for public', () => {
  assert.equal(minutesForRow(usageItem({ quantity: 250 }), 'private'), 250);
  assert.equal(minutesForRow(usageItem({ quantity: 250 }), 'unknown'), 250);
  assert.equal(minutesForRow(usageItem({ quantity: 250 }), 'public'), 0);
});

test('resolveRepositoryVisibility includes minutes on lookup failure', async () => {
  const github = fakeGitHub({ repoLookupFailure: { 'melodic-software/medley': true } });
  const cache = new Map();
  const visibility = await resolveRepositoryVisibility({
    github,
    owner: 'melodic-software',
    repo: 'medley',
    cache,
  });
  assert.equal(visibility, 'unknown');
  assert.equal(cache.get('melodic-software/medley'), 'unknown');
});

test('summarizeUsage counts raw quantity on private repos for standard hosted SKUs only', async () => {
  const github = fakeGitHub({
    repoVisibility: {
      'melodic-software/medley': true,
      'melodic-software/public-app': false,
    },
  });
  const summary = await summarizeUsage([
    usageItem({ sku: 'Actions Linux', quantity: 1000, repositoryName: 'medley' }),
    usageItem({ sku: 'Actions Windows', quantity: 200, repositoryName: 'medley' }),
    usageItem({ sku: 'Actions Linux', quantity: 500, repositoryName: 'public-app' }),
    usageItem({ sku: 'Actions Linux 16-core', quantity: 999, repositoryName: 'medley' }),
    usageItem({ product: 'packages', sku: 'Packages', unitType: 'GigabyteHours', quantity: 50 }),
  ], { includedMinutes: 3000, github });

  assert.equal(summary.consumedMinutes, 1200);
  assert.equal(summary.percentUsed, 40);
  assert.deepEqual(summary.perSku, [
    { sku: 'Actions Linux', minutes: 1000 },
    { sku: 'Actions Windows', minutes: 200 },
  ]);
});

test('summarizeUsage includes quantity when repository visibility lookup fails', async () => {
  const github = fakeGitHub({
    repoLookupFailure: { 'melodic-software/medley': true },
  });
  const summary = await summarizeUsage([usageItem({ quantity: 400 })], { includedMinutes: 3000, github });
  assert.equal(summary.consumedMinutes, 400);
});

test('summarizeUsage ignores discountAmount and uses quantity even when allowance did not pay', async () => {
  const github = fakeGitHub();
  const summary = await summarizeUsage([
    usageItem({ quantity: 300, discountAmount: 0, pricePerUnit: 0.008 }),
  ], { includedMinutes: 3000, github });
  assert.equal(summary.consumedMinutes, 300);
});

test('summarizeUsage drops rows missing required fields', async () => {
  const github = fakeGitHub();
  const summary = await summarizeUsage([
    usageItem({ quantity: 100 }),
    usageItem({ quantity: 200, repositoryName: '' }),
  ], { includedMinutes: 3000, github });
  assert.equal(summary.consumedMinutes, 100);
});

test('summarizeUsage refuses to report a quiet zero when Actions usage carries an unrecognized unit', async () => {
  await assert.rejects(
    () => summarizeUsage([usageItem({ unitType: 'compute-credits' })], { includedMinutes: 3000, github: fakeGitHub() }),
    /usage-report vocabulary changed/,
  );
});

test('summarizeUsage tolerates the documented field names under any value casing', async () => {
  const summary = await summarizeUsage(
    [usageItem({ product: 'Actions', unitType: 'Minutes' })],
    { includedMinutes: 3000, github: fakeGitHub() },
  );
  assert.equal(summary.consumedMinutes, 100);
});

test('summarizeUsage reports a clean month with no Actions usage at all', async () => {
  const summary = await summarizeUsage([], { includedMinutes: 3000, github: fakeGitHub() });
  assert.equal(summary.consumedMinutes, 0);
  assert.deepEqual(summary.breachedThresholds, []);
});

test('summarizeUsage breaches each configured threshold in order as consumption rises', async () => {
  const at = async (percent) => summarizeUsage(
    [usageItem({ quantity: 3000 * (percent / 100) })],
    { includedMinutes: 3000, github: fakeGitHub() },
  ).then(result => result.breachedThresholds);

  assert.deepEqual(POOL_ALERT_THRESHOLD_PERCENTS, [50, 80]);
  assert.deepEqual(await at(49.9), []);
  assert.deepEqual(await at(50), [50]);
  assert.deepEqual(await at(79.9), [50]);
  assert.deepEqual(await at(80), [50, 80]);
  assert.deepEqual(await at(140), [50, 80]);
});

test('summarizeUsage rejects a payload that is not the documented usageItems array', async () => {
  const github = fakeGitHub();
  await assert.rejects(() => summarizeUsage(undefined, { includedMinutes: 3000, github }), /must expose a usageItems array/);
  await assert.rejects(() => summarizeUsage({}, { includedMinutes: 3000, github }), /must expose a usageItems array/);
  await assert.rejects(() => summarizeUsage([], { includedMinutes: 0, github }), /positive number/);
  await assert.rejects(() => summarizeUsage([], { includedMinutes: Number.NaN, github }), /positive number/);
});

test('the measured summary carries no currency figure, keeping billing data out of this public repository', async () => {
  const summary = await summarizeUsage([
    usageItem({ pricePerUnit: 0.008, grossAmount: 12.34, discountAmount: 12.34 }),
    usageItem({ product: 'packages', unitType: 'GigabyteHours', netAmount: 99.99, quantity: 1 }),
  ], { includedMinutes: 3000, github: fakeGitHub() });

  const serialized = JSON.stringify(summary);
  for (const amount of ['12.34', '99.99', '0.008']) {
    assert.ok(!serialized.includes(amount), `the summary must not carry the currency figure ${amount}`);
  }
});

test('billingMonth resolves the UTC billing month regardless of local time', () => {
  assert.deepEqual(billingMonth(Date.parse('2026-07-31T23:30:00Z')), { year: 2026, month: 7 });
  assert.deepEqual(billingMonth(Date.parse('2026-08-01T00:30:00Z')), { year: 2026, month: 8 });
  assert.equal(formatBillingMonth({ year: 2026, month: 8 }), '2026-08');
});

test('run() queries the documented usage endpoint for the current billing month', async () => {
  const github = fakeGitHub({ usageItems: [usageItem()] });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      BILLING_ORG: 'melodic-software',
      INCLUDED_MINUTES: '3000',
      BILLING_TOKEN_PRESENT: 'true',
      BUDGET_MONITOR_ARMED: 'true',
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const [, route, parameters] = github.calls.find(([action]) => action === 'request');
  assert.equal(route, 'GET /organizations/{org}/settings/billing/usage');
  assert.deepEqual(parameters, { org: 'melodic-software', year: 2026, month: 7 });
  assert.deepEqual(core.calls.setFailed, []);
  const [name, value] = core.calls.setOutput[0];
  assert.equal(name, 'budget');
  assert.deepEqual(JSON.parse(value).month, '2026-07');
});

test('run() skips with a warning when the monitor is disarmed', async () => {
  const github = fakeGitHub({ usageItems: [usageItem()] });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      BILLING_ORG: 'melodic-software',
      INCLUDED_MINUTES: '3000',
      BILLING_TOKEN_PRESENT: 'true',
      BUDGET_MONITOR_ARMED: 'false',
    },
  });

  assert.equal(core.calls.warning.length, 1);
  assert.match(core.calls.warning[0], /disarmed/);
  assert.deepEqual(core.calls.setOutput, []);
  assert.deepEqual(core.calls.setFailed, []);
  assert.equal(github.calls.filter(([action]) => action === 'request').length, 0);
});

test('run() fails loudly when armed but the billing token is not provisioned', async () => {
  const github = fakeGitHub({ usageItems: [] });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      BILLING_ORG: 'melodic-software',
      INCLUDED_MINUTES: '3000',
      BILLING_TOKEN_PRESENT: 'false',
      BUDGET_MONITOR_ARMED: 'true',
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  assert.equal(core.calls.setFailed.length, 1);
  assert.match(core.calls.setFailed[0], /CI_RUNNER_BILLING_OBSERVER_TOKEN/);
  assert.deepEqual(core.calls.setOutput, []);
  assert.equal(github.calls.filter(([action]) => action === 'request').length, 0);
});

test('run() propagates a genuine execution error via setFailed and skips the budget output', async () => {
  const github = fakeGitHub({ requestFailure: true });
  const core = fakeCore();
  await run({
    github,
    core,
    env: {
      BILLING_ORG: 'melodic-software',
      INCLUDED_MINUTES: '3000',
      BILLING_TOKEN_PRESENT: 'true',
      BUDGET_MONITOR_ARMED: 'true',
    },
  });

  assert.equal(core.calls.setFailed.length, 1);
  assert.deepEqual(core.calls.setOutput, []);
});

test('run() rejects incomplete configuration', async () => {
  const core = fakeCore();
  await run({
    github: fakeGitHub(),
    core,
    env: { INCLUDED_MINUTES: '3000', BILLING_TOKEN_PRESENT: 'true', BUDGET_MONITOR_ARMED: 'true' },
  });
  assert.match(core.calls.setFailed[0], /BILLING_ORG is required/);

  const secondCore = fakeCore();
  await run({
    github: fakeGitHub(),
    core: secondCore,
    env: {
      BILLING_ORG: 'melodic-software',
      INCLUDED_MINUTES: 'not-a-number',
      BILLING_TOKEN_PRESENT: 'true',
      BUDGET_MONITOR_ARMED: 'true',
    },
  });
  assert.match(secondCore.calls.setFailed[0], /positive number/);
});

test('renderSkuMarkdownTable keeps a stable shape, caps rows, and escapes untrusted SKU text', () => {
  const table = renderSkuMarkdownTable([{ sku: 'Actions Linux', minutes: 1500 }]);
  assert.match(table, /^\| SKU \| Included minutes consumed \|/);
  assert.match(table, /\| Actions Linux \| 1500 \|/);

  const injected = renderSkuMarkdownTable([{ sku: `evil ${poolIncidentMarker('owner-b')} | name`, minutes: 1 }]);
  assert.ok(!injected.includes(poolIncidentMarker('owner-b')), 'a crafted SKU must not smuggle a functional marker into the body');
  assert.match(injected, /\\\|/);

  const many = Array.from({ length: MAX_SKU_TABLE_ROWS + 4 }, (_, index) => ({ sku: `sku-${index}`, minutes: 1 }));
  const capped = renderSkuMarkdownTable(many);
  assert.equal(capped.split('\n').filter(line => line.startsWith('| sku-')).length, MAX_SKU_TABLE_ROWS);
  assert.match(capped, /_\.\.\.and 4 more SKU\(s\)\._/);
});

test('this monitor\'s markers cannot collide with the queue monitor\'s markers for the same owner', () => {
  const owner = 'melodic-software';
  const markers = [poolIncidentMarker(owner), queueIncidentMarker(owner)];
  for (const left of markers) {
    for (const right of markers) {
      if (left === right) continue;
      assert.ok(!left.includes(right), `'${left}' must not contain '${right}' as a substring`);
    }
  }
});

test('marker and title shapes stay greppable and prefix-safe across owners', () => {
  assert.equal(poolIncidentTitle('melodic-software'), '[Alert] Actions included-minute consumption — melodic-software');
  assert.ok(!poolIncidentMarker('melodic-software-fork').includes(poolIncidentMarker('melodic-software')));
});

test('recovery guidance names free capacity for the pool', () => {
  assert.match(poolRecoverySummary, /free for public repositories/);
  assert.match(poolRecoverySummary, /self-hosted runners consume no included minutes/);
  assert.match(poolRecoverySummary, /new billing month resets the allowance/);
});

test('upsertIncident opens a pool incident carrying the breached tier markers', async () => {
  const github = fakeGitHub();
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { ...baseUpsertEnv, BUDGET_JSON: JSON.stringify(budgetFixture()) },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const created = github.calls.find(([action]) => action === 'create');
  const [, parameters] = created;
  assert.equal(parameters.title, poolIncidentTitle('melodic-software'));
  assert.deepEqual(parameters.labels, ['automated']);
  assert.ok(parameters.body.includes(tierMarker(50)));
  assert.ok(!parameters.body.includes(tierMarker(80)));
  assert.ok(parameters.body.endsWith(poolIncidentMarker('melodic-software')));
  assert.match(parameters.body, /1500 of 3000 included minute\(s\) consumed \(50%\)/);
});

test('upsertIncident silently updates a pool incident that is still at the same tier', async () => {
  const marker = poolIncidentMarker('melodic-software');
  const github = fakeGitHub({
    existingIssues: [ownIssue({ number: 55, body: `${tierMarker(50)}\nstale ${marker}` })],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { ...baseUpsertEnv, BUDGET_JSON: JSON.stringify(budgetFixture({ consumedMinutes: 1800, percentUsed: 60 })) },
    now: Date.parse('2026-07-23T10:00:00Z'),
  });

  assert.equal(github.calls.filter(([action]) => action === 'update').length, 1);
  assert.equal(github.calls.filter(([action]) => action === 'createComment').length, 0);
});

test('upsertIncident comments once when the pool incident escalates to a higher tier', async () => {
  const marker = poolIncidentMarker('melodic-software');
  const github = fakeGitHub({
    existingIssues: [ownIssue({ number: 55, body: `${tierMarker(50)}\nstale ${marker}` })],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      ...baseUpsertEnv,
      BUDGET_JSON: JSON.stringify(budgetFixture({ consumedMinutes: 2460, percentUsed: 82, breachedThresholds: [50, 80] })),
    },
    now: Date.parse('2026-07-24T10:00:00Z'),
  });

  const updated = github.calls.find(([action]) => action === 'update');
  assert.ok(updated[1].body.includes(tierMarker(80)));
  const comments = github.calls.filter(([action]) => action === 'createComment');
  assert.equal(comments.length, 1);
  assert.match(comments[0][1].body, /Escalated to 80%/);
});

test('upsertIncident closes the pool incident when a new billing month resets consumption', async () => {
  const marker = poolIncidentMarker('melodic-software');
  const github = fakeGitHub({
    existingIssues: [ownIssue({ number: 55, body: `${tierMarker(50)}${tierMarker(80)}\nstale ${marker}` })],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      ...baseUpsertEnv,
      BUDGET_JSON: JSON.stringify(budgetFixture({ month: '2026-08', consumedMinutes: 12, percentUsed: 0.4, breachedThresholds: [] })),
    },
    now: Date.parse('2026-08-01T10:00:00Z'),
  });

  const commented = github.calls.find(([action]) => action === 'createComment');
  const updated = github.calls.find(([action]) => action === 'update');
  assert.match(commented[1].body, /under every alert threshold/);
  assert.equal(updated[1].issue_number, 55);
  assert.equal(updated[1].state, 'closed');
  assert.equal(updated[1].state_reason, 'completed');
});

test('upsertIncident skips when disarmed', async () => {
  const github = fakeGitHub();
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { ...baseUpsertEnv, BUDGET_MONITOR_ARMED: 'false', BUDGET_JSON: JSON.stringify(budgetFixture()) },
  });

  assert.equal(core.calls.warning.length, 1);
  assert.deepEqual(github.calls.filter(([action]) => action !== 'paginate'), []);
});

test('upsertIncident is a no-op when nothing is breached and no incident is open', async () => {
  const github = fakeGitHub();
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { ...baseUpsertEnv, BUDGET_JSON: JSON.stringify(budgetFixture({ breachedThresholds: [] })) },
    now: Date.parse('2026-08-01T10:00:00Z'),
  });

  assert.deepEqual(github.calls.filter(([action]) => action !== 'paginate'), []);
});

test('upsertIncident does not adopt a decoy issue carrying the marker under another author', async () => {
  const github = fakeGitHub({
    existingIssues: [{ number: 66, body: `decoy ${poolIncidentMarker('melodic-software')}`, pull_request: undefined, user: { login: 'kyle-sexton', type: 'User' } }],
  });
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: { ...baseUpsertEnv, BUDGET_JSON: JSON.stringify(budgetFixture()) },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  assert.ok(github.calls.find(([action]) => action === 'create'));
  assert.equal(github.calls.filter(([action]) => action === 'update').length, 0);
});

test('upsertIncident rejects a missing home repository or issue-author login', async () => {
  const github = fakeGitHub();
  const core = fakeCore();
  const budgetJson = JSON.stringify(budgetFixture());
  await assert.rejects(
    upsertIncident({ github, core, env: { ISSUE_AUTHOR_LOGIN, BUDGET_JSON: budgetJson, BUDGET_MONITOR_ARMED: 'true' } }),
    /GITHUB_REPOSITORY must be set/,
  );
  await assert.rejects(
    upsertIncident({ github, core, env: { GITHUB_REPOSITORY: 'melodic-software/ci-runner', BUDGET_JSON: budgetJson, BUDGET_MONITOR_ARMED: 'true' } }),
    /ISSUE_AUTHOR_LOGIN is required/,
  );
});

test('parseBudget rejects a missing, malformed, or structurally wrong payload instead of assuming recovery', async () => {
  assert.throws(() => parseBudget(undefined), /BUDGET_JSON is required/);
  assert.throws(() => parseBudget(''), /BUDGET_JSON is required/);
  assert.throws(() => parseBudget('{not json'), /not valid JSON/);
  assert.throws(() => parseBudget('[]'), /must decode to an object/);
  assert.throws(() => parseBudget('{}'), /breachedThresholds/);

  const github = fakeGitHub({
    existingIssues: [ownIssue({ number: 55, body: `open ${poolIncidentMarker('melodic-software')}` })],
  });
  await assert.rejects(upsertIncident({ github, core: fakeCore(), env: baseUpsertEnv }), /BUDGET_JSON is required/);
  assert.equal(github.calls.filter(([action]) => action === 'update' || action === 'createComment').length, 0);
});

test('pool alert bodies publish no currency amounts', async () => {
  const github = fakeGitHub();
  const core = fakeCore();
  await upsertIncident({
    github,
    core,
    env: {
      ...baseUpsertEnv,
      BUDGET_JSON: JSON.stringify(budgetFixture({ breachedThresholds: [50, 80] })),
    },
    now: Date.parse('2026-07-22T10:00:00Z'),
  });

  const created = github.calls.find(([action]) => action === 'create');
  const body = created[1].body;
  for (const amount of body.match(/\$\d+(?:\.\d+)?/g) ?? []) {
    assert.equal(amount, '$0', `no currency amount may reach a public issue body (found ${amount})`);
  }
});
