'use strict';

const nonterminalRunStatuses = Object.freeze([
  'queued',
  'in_progress',
  'requested',
  'waiting',
  'pending',
]);

const routingRecoverySummary = `
Follow the [audited CI routing-control procedure](https://github.com/melodic-software/github-iac/blob/main/README.md#local-ci-routing-governance) to make the affected repository's effective \`CI_RUNNER_POLICY\` value \`hosted-only\` and verify the readback. Cancel the affected run, choose **Re-run all jobs** to guarantee that the selector executes again, and confirm that it selects hosted capacity. Do not use a failed-job or single-job rerun for this recovery because partial-rerun dependency behavior does not guarantee a fresh selector decision. A \`workflow_dispatch\` creates a separate run with different event and ref context; it does not recover the original pull-request check.
`;
const routingRecoveryFailure = 'Set the effective CI_RUNNER_POLICY value to hosted-only through audited routing control, verify it, then use Re-run all jobs and confirm hosted selection.';

function splitList(value) {
  return (value || '')
    .split(/[\s,]+/)
    .map(item => item.trim())
    .filter(Boolean);
}

async function inspectQueuedJobs({
  github,
  owner,
  repositories,
  managedLabels,
  thresholdMinutes,
  now = Date.now(),
}) {
  if (!owner || repositories.length === 0 || managedLabels.size === 0) {
    throw new Error('Queue monitor configuration is incomplete: owner, repositories, and managed labels are required.');
  }
  if (!Number.isFinite(thresholdMinutes) || thresholdMinutes <= 0) {
    throw new Error('Queue monitor threshold must be a positive number of minutes.');
  }

  const cutoff = now - thresholdMinutes * 60 * 1000;
  const stuck = [];
  for (const repo of repositories) {
    const runsById = new Map();
    for (const status of nonterminalRunStatuses) {
      const runs = await github.paginate(github.rest.actions.listWorkflowRunsForRepo, {
        owner,
        repo,
        status,
        per_page: 100,
      });
      for (const run of runs) runsById.set(run.id, run);
    }

    for (const run of runsById.values()) {
      const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
        owner,
        repo,
        run_id: run.id,
        filter: 'latest',
        per_page: 100,
      });
      for (const job of jobs) {
        const labels = job.labels || [];
        const isManaged = labels.some(label => managedLabels.has(label));
        const queuedAt = Date.parse(job.created_at);
        if (job.status === 'queued' && isManaged && Number.isFinite(queuedAt) && queuedAt <= cutoff) {
          stuck.push({
            repository: `${owner}/${repo}`,
            workflow: run.name || run.path,
            job: job.name,
            queuedMinutes: Math.floor((now - queuedAt) / 60000),
            url: job.html_url || run.html_url,
            labels: labels.join(', '),
          });
        }
      }
    }
  }
  return stuck;
}

async function run({ github, core, env = process.env, now = Date.now() }) {
  const owner = env.MONITOR_OWNER;
  const repositories = splitList(env.MONITORED_REPOSITORIES);
  const managedLabels = new Set(splitList(env.MANAGED_LABELS));
  const thresholdMinutes = Number(env.QUEUE_THRESHOLD_MINUTES);

  let stuck;
  try {
    stuck = await inspectQueuedJobs({
      github,
      owner,
      repositories,
      managedLabels,
      thresholdMinutes,
      now,
    });
  } catch (error) {
    core.setFailed(error instanceof Error ? error.message : String(error));
    return;
  }

  if (stuck.length === 0) {
    await core.summary.addHeading('Managed runner queue').addRaw('No managed job has been queued for more than five minutes.').write();
    return;
  }

  const rows = stuck.map(item => [
    item.repository,
    item.workflow,
    item.job,
    String(item.queuedMinutes),
    item.labels,
    `[open job](${item.url})`,
  ]);
  await core.summary
    .addHeading('Managed runner queue alert')
    .addTable([
      [{ data: 'Repository', header: true }, { data: 'Workflow', header: true }, { data: 'Job', header: true }, { data: 'Minutes', header: true }, { data: 'Labels', header: true }, { data: 'Link', header: true }],
      ...rows,
    ])
    .addRaw(routingRecoverySummary)
    .write();
  core.setFailed(`${stuck.length} managed job(s) exceeded the five-minute queue threshold. ${routingRecoveryFailure}`);
}

module.exports = {
  inspectQueuedJobs,
  nonterminalRunStatuses,
  routingRecoveryFailure,
  routingRecoverySummary,
  run,
  splitList,
};
