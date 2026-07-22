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

  // Handed to the incident-upsert step via job output; a successful execution
  // stays green from here regardless of what it found (see upsertIncident).
  core.setOutput('stuck', JSON.stringify(stuck));

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
}

function incidentTitle(targetOwner) {
  return `[Alert] Managed runner queue capacity — ${targetOwner}`;
}

function incidentMarker(targetOwner) {
  return `<!-- ci-runner:queued-job-monitor:incident:${targetOwner} -->`;
}

// Neutralizes '<' and '>' (not just '|') because this cell content is
// untrusted: it comes from monitored-repo job/workflow names, not this
// workflow's own source. Left unescaped, a crafted job name could inject a
// literal '<!-- ... -->' sequence into a bot-authored incident body —
// including another owner's incident marker, which findOpenIncident matches
// as a raw substring — causing a cross-owner issue collision. HTML-entity
// encoding renders as the literal characters (GitHub's Markdown renders
// '&lt;'/'&gt;' back to '<'/'>' visually) while never forming a real '<!--'
// or '-->' sequence in the raw body text this workflow's own code searches.
function escapeMarkdownTableCell(value) {
  return String(value)
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\|/g, '\\|');
}

function renderStuckMarkdownTable(stuck) {
  const header = '| Repository | Workflow | Job | Minutes | Labels | Link |\n| --- | --- | --- | --- | --- | --- |';
  const rows = stuck.map(item => `| ${escapeMarkdownTableCell(item.repository)} | ${escapeMarkdownTableCell(item.workflow)} | ${escapeMarkdownTableCell(item.job)} | ${item.queuedMinutes} | ${escapeMarkdownTableCell(item.labels)} | [open job](${item.url}) |`);
  return [header, ...rows].join('\n');
}

function isOwnIncidentAuthor(issue, issueAuthorLogin) {
  return issue.user?.login === issueAuthorLogin && issue.user?.type === 'Bot';
}

// The marker and title strings are public (embedded verbatim in this
// workflow's source, in a public repository), so matching on them alone
// would let anyone with issue-creation permission here craft a decoy this
// automation adopts, silently updates, or closes as recovered — suppressing
// a real alert. Restricting candidates to issues opened by this workflow's
// own token identity closes that: an attacker's issue is never a candidate,
// no matter what text it carries. Mirrors the hardening in ci-workflows#213
// (standards-sync-stuck-automerge-alert.yml). Ambiguity (more than one
// candidate carrying the marker) fails closed rather than guessing.
//
// Matching stays marker-only (not marker + title), matching the fleet
// precedent's deliberate "a marker survives a retitle" property. A crafted
// job name in a monitored repo could in principle try to inject a foreign
// marker into a bot-authored body; escapeMarkdownTableCell neutralizes that
// at the source (see its own comment) instead of layering a title guard here
// that would trade retitle-survival for redundant protection.
async function findOpenIncident({ github, homeOwner, homeRepo, marker, issueAuthorLogin }) {
  // Every incident issue this workflow creates carries the 'automated' label
  // (see upsertIncident's create call); filtering server-side keeps this
  // ~15-minutes-while-open scan from paginating every open issue in the repo.
  const openIssues = await github.paginate(github.rest.issues.listForRepo, {
    owner: homeOwner,
    repo: homeRepo,
    state: 'open',
    labels: 'automated',
    per_page: 100,
  });
  const candidates = openIssues.filter(issue => !issue.pull_request && isOwnIncidentAuthor(issue, issueAuthorLogin));
  const matches = candidates.filter(issue => (issue.body ?? '').includes(marker));
  if (matches.length > 1) {
    throw new Error(`Found ${matches.length} open incident issues carrying marker '${marker}'; reconcile manually.`);
  }
  return matches[0] || null;
}

// Marker-deduped issue-per-incident alert channel (the fleet's established
// pattern; see link-check.yml and queue-monitor-liveness.yml in ci-workflows).
// Runs on the job's own GITHUB_TOKEN against the monitor's home repository —
// distinct from the read-only, target-scoped observer token the detection
// step uses, which cannot write issues here. Any thrown error here fails the
// run: an incident-issue write failure is the monitor breaking, not a queue
// alert.
async function upsertIncident({ github, core, env = process.env, now = Date.now() }) {
  const targetOwner = env.TARGET_OWNER;
  const [homeOwner, homeRepo] = (env.GITHUB_REPOSITORY || '').split('/');
  const issueAuthorLogin = env.ISSUE_AUTHOR_LOGIN;
  if (!homeOwner || !homeRepo) {
    throw new Error('GITHUB_REPOSITORY must be set to the owner/repo of the monitor workflow.');
  }
  if (!targetOwner) {
    throw new Error('TARGET_OWNER is required to key the incident issue.');
  }
  if (!issueAuthorLogin) {
    throw new Error('ISSUE_AUTHOR_LOGIN is required to restrict incident-issue adoption to this workflow\'s own identity.');
  }
  // A missing or empty STUCK_JSON is never "zero stuck jobs" — that case is
  // always an explicit "[]" from run()'s core.setOutput. Falling back to '[]'
  // here would silently treat a missing detection-step output (a wiring bug,
  // a skipped step) as a healthy recovery and close a real open incident.
  if (env.STUCK_JSON === undefined || env.STUCK_JSON === '') {
    throw new Error('STUCK_JSON is required: the detection step must set it via core.setOutput, even for zero stuck jobs.');
  }
  let stuck;
  try {
    stuck = JSON.parse(env.STUCK_JSON);
  } catch (error) {
    throw new Error(`STUCK_JSON is not valid JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!Array.isArray(stuck)) {
    throw new Error('STUCK_JSON must decode to an array.');
  }

  const title = incidentTitle(targetOwner);
  const marker = incidentMarker(targetOwner);
  const existing = await findOpenIncident({ github, homeOwner, homeRepo, marker, issueAuthorLogin });
  const nowIso = new Date(now).toISOString();

  if (stuck.length === 0) {
    if (!existing) {
      core.info(`No open incident for ${targetOwner}; queue is healthy.`);
      return;
    }
    await github.rest.issues.createComment({
      owner: homeOwner,
      repo: homeRepo,
      issue_number: existing.number,
      body: `Recovered: no managed job for \`${targetOwner}\` has been queued past the threshold as of ${nowIso}. Capacity window: ${existing.created_at} – ${nowIso}.`,
    });
    await github.rest.issues.update({
      owner: homeOwner,
      repo: homeRepo,
      issue_number: existing.number,
      state: 'closed',
      state_reason: 'completed',
    });
    core.info(`Closed incident #${existing.number} for ${targetOwner}.`);
    return;
  }

  const windowStart = existing ? existing.created_at : nowIso;
  const body = [
    `Managed runner queue capacity alert for \`${targetOwner}\`.`,
    '',
    `Capacity window: constrained since ${windowStart} (last confirmed ${nowIso}). Affected queue depth: ${stuck.length} managed job(s).`,
    '',
    renderStuckMarkdownTable(stuck),
    '',
    routingRecoverySummary.trim(),
    '',
    marker,
  ].join('\n');

  if (existing) {
    // A silent body update, not a comment: this step runs every ~15 minutes
    // while an incident stays open, and GitHub notifies watchers on every
    // comment but not on a body edit. Commenting here would re-create the
    // notification noise this alert channel replaces. The body's "last
    // confirmed" timestamp already carries freshness.
    await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body });
    core.info(`Updated incident #${existing.number} for ${targetOwner}.`);
  } else {
    const created = await github.rest.issues.create({ owner: homeOwner, repo: homeRepo, title, body, labels: ['automated'] });
    core.info(`Opened incident #${created.data.number} for ${targetOwner}.`);
  }
}

module.exports = {
  findOpenIncident,
  incidentMarker,
  incidentTitle,
  inspectQueuedJobs,
  nonterminalRunStatuses,
  renderStuckMarkdownTable,
  routingRecoverySummary,
  run,
  splitList,
  upsertIncident,
};
