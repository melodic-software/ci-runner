'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  inspectQueuedJobs,
  nonterminalRunStatuses,
  routingRecoveryFailure,
  routingRecoverySummary,
  splitList,
} = require('./queue-monitor.cjs');

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
  assert.match(routingRecoveryFailure, /Re-run all jobs/);
  assert.doesNotMatch(routingRecoveryFailure, /workflow_dispatch/);
  assert.doesNotMatch(routingRecoverySummary, /retry(?:ing)? (?:the )?workload/i);
  assert.doesNotMatch(routingRecoveryFailure, /retry(?:ing)? (?:the )?workload/i);
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
