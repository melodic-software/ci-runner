'use strict';

const {
  boundBodyLength,
  escapeMarkdownTableCell,
  findOpenIncident,
} = require('./incident-issue.cjs');

// Percentages of the included-minute allowance that open a pool incident. Both
// stay breached once crossed within a month (consumption only rises until the
// allowance resets), so the body reports the highest tier reached and the
// escalation between them is a comment, not a second issue.
const POOL_ALERT_THRESHOLD_PERCENTS = Object.freeze([50, 80]);

const ACTIONS_PRODUCT = 'actions';
const MINUTE_UNIT_TYPES = Object.freeze(['minutes', 'minute']);

// Standard hosted runner SKUs only — excludes larger runners, self-hosted, and
// non-minute Actions products.
const STANDARD_HOSTED_SKUS = Object.freeze([
  'Actions Linux',
  'Actions Linux Slim',
  'Actions Windows',
]);

const MAX_SKU_TABLE_ROWS = 25;

function isActionsMinuteItem(item) {
  return String(item.product ?? '').toLowerCase() === ACTIONS_PRODUCT
    && MINUTE_UNIT_TYPES.includes(String(item.unitType ?? '').toLowerCase());
}

function isStandardHostedSku(item) {
  return STANDARD_HOSTED_SKUS.includes(String(item.sku ?? ''));
}

function hasRequiredFields(item) {
  const quantity = Number(item.quantity);
  return Boolean(item.organizationName)
    && Boolean(item.repositoryName)
    && Boolean(item.sku)
    && Number.isFinite(quantity)
    && quantity >= 0;
}

function roundMinutes(minutes) {
  return Math.round(minutes * 10) / 10;
}

function billingMonth(now) {
  const date = new Date(now);
  return { year: date.getUTCFullYear(), month: date.getUTCMonth() + 1 };
}

function formatBillingMonth({ year, month }) {
  return `${year}-${String(month).padStart(2, '0')}`;
}

async function resolveRepositoryVisibility({ github, owner, repo, cache }) {
  const key = `${owner}/${repo}`;
  if (cache.has(key)) return cache.get(key);
  try {
    const response = await github.rest.repos.get({ owner, repo });
    const visibility = response.data.private ? 'private' : 'public';
    cache.set(key, visibility);
    return visibility;
  } catch {
    // Frontier tiebreak (#171): when visibility is unknown — 404, permission
    // error, or any other lookup failure — include the row's minutes rather
    // than drop them. Under-counting would suppress alerts; over-counting is
    // the safer failure mode for a budget watchdog.
    cache.set(key, 'unknown');
    return 'unknown';
  }
}

function minutesForRow(item, visibility) {
  if (visibility === 'public') return 0;
  const quantity = Number(item.quantity);
  if (!Number.isFinite(quantity) || quantity <= 0) return 0;
  return quantity;
}

async function summarizeUsage(usageItems, { includedMinutes, github }) {
  if (!Array.isArray(usageItems)) {
    throw new Error('The billing usage report must expose a usageItems array.');
  }
  if (!Number.isFinite(includedMinutes) || includedMinutes <= 0) {
    throw new Error('Included minutes must be a positive number.');
  }

  const actionsItems = usageItems.filter(isActionsMinuteItem);
  const eligibleItems = actionsItems.filter(item => isStandardHostedSku(item) && hasRequiredFields(item));

  // Fail loud only when a standard hosted runner SKU reports a non-minute unit —
  // that is a vocabulary change for the rows this monitor counts. Legitimate
  // non-minute Actions products (storage, etc.) are excluded and must not block
  // a zero-consumption month before any runner-minute row exists.
  const runnerSkusWithUnexpectedUnit = usageItems.filter(
    item => String(item.product ?? '').toLowerCase() === ACTIONS_PRODUCT
      && isStandardHostedSku(item)
      && !isActionsMinuteItem(item),
  );
  if (runnerSkusWithUnexpectedUnit.length > 0) {
    throw new Error('A standard hosted Actions runner SKU was reported without a minute unit type; the usage-report vocabulary changed.');
  }

  const visibilityCache = new Map();
  const minutesBySku = new Map();
  for (const item of eligibleItems) {
    const visibility = await resolveRepositoryVisibility({
      github,
      owner: item.organizationName,
      repo: item.repositoryName,
      cache: visibilityCache,
    });
    const minutes = minutesForRow(item, visibility);
    if (minutes <= 0) continue;
    const sku = String(item.sku);
    minutesBySku.set(sku, (minutesBySku.get(sku) || 0) + minutes);
  }

  const consumedMinutes = [...minutesBySku.values()].reduce((total, minutes) => total + minutes, 0);
  const percentUsed = (consumedMinutes / includedMinutes) * 100;

  return {
    includedMinutes,
    consumedMinutes: roundMinutes(consumedMinutes),
    percentUsed: Math.round(percentUsed * 10) / 10,
    breachedThresholds: POOL_ALERT_THRESHOLD_PERCENTS.filter(threshold => percentUsed >= threshold),
    perSku: [...minutesBySku.entries()]
      .map(([sku, minutes]) => ({ sku, minutes: roundMinutes(minutes) }))
      .sort((left, right) => right.minutes - left.minutes || left.sku.localeCompare(right.sku)),
  };
}

async function fetchUsage({ github, org, year, month }) {
  const response = await github.request('GET /organizations/{org}/settings/billing/usage', {
    org,
    year,
    month,
  });
  return response.data?.usageItems;
}

async function run({ github, core, env = process.env, now = Date.now() }) {
  const org = env.BILLING_ORG;
  const includedMinutes = Number(env.INCLUDED_MINUTES);

  if (env.BUDGET_MONITOR_ARMED !== 'true') {
    core.warning('Actions budget monitor is disarmed (BUDGET_MONITOR_ARMED != true); skipping measurement.');
    return;
  }

  let summary;
  let month;
  try {
    if (!org) {
      throw new Error('BILLING_ORG is required to scope the billing usage report.');
    }
    if (env.BILLING_TOKEN_PRESENT !== 'true') {
      throw new Error('The billing observer token is not provisioned: set the CI_RUNNER_BILLING_OBSERVER_TOKEN secret. Refusing to report consumption without it.');
    }
    month = billingMonth(now);
    summary = await summarizeUsage(await fetchUsage({ github, org, ...month }), { includedMinutes, github });
  } catch (error) {
    core.setFailed(error instanceof Error ? error.message : String(error));
    return;
  }

  const budget = { org, month: formatBillingMonth(month), ...summary };

  core.setOutput('budget', JSON.stringify(budget));

  await core.summary
    .addHeading('Actions included-minute consumption')
    .addRaw(`\`${org}\` used ${budget.consumedMinutes} of ${includedMinutes} included minute(s) in ${budget.month} (${budget.percentUsed}%).`)
    .write();
}

function poolIncidentTitle(org) {
  return `[Alert] Actions included-minute consumption — ${org}`;
}

function poolIncidentMarker(org) {
  return `<!-- ci-runner:actions-budget-monitor:incident:${org}:pool -->`;
}

function tierMarker(month, threshold) {
  return `<!-- ci-runner:actions-budget-monitor:tier:${month}:${threshold} -->`;
}

function renderSkuMarkdownTable(perSku, { maxRows = MAX_SKU_TABLE_ROWS } = {}) {
  const header = '| SKU | Included minutes consumed |\n| --- | --- |';
  const shown = perSku.slice(0, maxRows);
  const rows = shown.map(entry => `| ${escapeMarkdownTableCell(entry.sku)} | ${entry.minutes} |`);
  const lines = [header, ...rows];
  const remaining = perSku.length - shown.length;
  if (remaining > 0) {
    lines.push('', `_...and ${remaining} more SKU(s)._`);
  }
  return lines.join('\n');
}

const poolRecoverySummary = `
Included-minute consumption is a hard-\`$0\` posture signal, not a runner-capacity failure. Confirm which repositories and workflows are drawing on the allowance, and move eligible work onto free capacity: standard GitHub-hosted runners are free for public repositories, and self-hosted runners consume no included minutes. A new billing month resets the allowance and closes this incident on its own.
`;

function parseBudget(budgetJson) {
  if (budgetJson === undefined || budgetJson === '') {
    throw new Error('BUDGET_JSON is required: the measurement step must set it via core.setOutput.');
  }
  let budget;
  try {
    budget = JSON.parse(budgetJson);
  } catch (error) {
    throw new Error(`BUDGET_JSON is not valid JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (budget === null || typeof budget !== 'object' || Array.isArray(budget)) {
    throw new Error('BUDGET_JSON must decode to an object.');
  }
  if (!Array.isArray(budget.breachedThresholds)) {
    throw new Error('BUDGET_JSON must carry a breachedThresholds array.');
  }
  return budget;
}

async function closeIncident({ github, homeOwner, homeRepo, core, existing, recoveryBody }) {
  await github.rest.issues.createComment({
    owner: homeOwner,
    repo: homeRepo,
    issue_number: existing.number,
    body: recoveryBody,
  });
  await github.rest.issues.update({
    owner: homeOwner,
    repo: homeRepo,
    issue_number: existing.number,
    state: 'closed',
    state_reason: 'completed',
  });
  core.info(`Closed incident #${existing.number}.`);
}

function reportedThresholdsForMonth(body, month, breachedThresholds) {
  return breachedThresholds.filter(threshold => (body ?? '').includes(tierMarker(month, threshold)));
}

function buildPoolIncidentBody({ budget, nowIso, reportedThresholds, marker }) {
  const highestThreshold = Math.max(...budget.breachedThresholds);
  const tierMarkers = reportedThresholds.map(threshold => tierMarker(budget.month, threshold)).join('');
  const bodyWithoutMarker = [
    tierMarkers,
    '',
    `Actions included-minute consumption alert for \`${budget.org}\`: ${highestThreshold}% of the included allowance reached.`,
    '',
    `Billing month ${budget.month}: ${budget.consumedMinutes} of ${budget.includedMinutes} included minute(s) consumed (${budget.percentUsed}%). Last confirmed ${nowIso}.`,
    '',
    renderSkuMarkdownTable(budget.perSku),
    '',
    poolRecoverySummary.trim(),
  ].join('\n');
  return boundBodyLength(bodyWithoutMarker, marker);
}

async function upsertPoolIncident({ github, core, homeOwner, homeRepo, issueAuthorLogin, budget, nowIso }) {
  const marker = poolIncidentMarker(budget.org);
  const existing = await findOpenIncident({ github, homeOwner, homeRepo, marker, issueAuthorLogin });

  if (budget.breachedThresholds.length === 0) {
    if (!existing) {
      core.info(`No open pool incident for ${budget.org}; included-minute consumption is under every threshold.`);
      return;
    }
    await closeIncident({
      github,
      homeOwner,
      homeRepo,
      core,
      existing,
      recoveryBody: `Recovered: \`${budget.org}\` has consumed ${budget.consumedMinutes} of ${budget.includedMinutes} included minute(s) in ${budget.month} (${budget.percentUsed}%), under every alert threshold, as of ${nowIso}.`,
    });
    return;
  }

  const highestThreshold = Math.max(...budget.breachedThresholds);

  if (!existing) {
    const body = buildPoolIncidentBody({
      budget,
      nowIso,
      reportedThresholds: budget.breachedThresholds,
      marker,
    });
    const created = await github.rest.issues.create({
      owner: homeOwner,
      repo: homeRepo,
      title: poolIncidentTitle(budget.org),
      body,
      labels: ['automated'],
    });
    core.info(`Opened pool incident #${created.data.number} for ${budget.org}.`);
    return;
  }

  const reportedThresholds = reportedThresholdsForMonth(existing.body, budget.month, budget.breachedThresholds);
  const escalatedThresholds = budget.breachedThresholds.filter(threshold => !reportedThresholds.includes(threshold));

  if (escalatedThresholds.length === 0) {
    const body = buildPoolIncidentBody({
      budget,
      nowIso,
      reportedThresholds: budget.breachedThresholds,
      marker,
    });
    await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body });
    core.info(`Updated pool incident #${existing.number} for ${budget.org}.`);
    return;
  }

  // Retry-safe escalation: refresh the body and notify before persisting tier
  // markers for thresholds whose comment has not yet succeeded. If the comment
  // fails, the next run still sees the threshold as unreported for this month.
  const bodyBeforeEscalation = buildPoolIncidentBody({
    budget,
    nowIso,
    reportedThresholds,
    marker,
  });
  await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body: bodyBeforeEscalation });
  await github.rest.issues.createComment({
    owner: homeOwner,
    repo: homeRepo,
    issue_number: existing.number,
    body: `Escalated to ${highestThreshold}% of the included allowance: ${budget.consumedMinutes} of ${budget.includedMinutes} minute(s) consumed in ${budget.month} as of ${nowIso}.`,
  });
  const bodyAfterEscalation = buildPoolIncidentBody({
    budget,
    nowIso,
    reportedThresholds: budget.breachedThresholds,
    marker,
  });
  await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body: bodyAfterEscalation });
  core.info(`Updated pool incident #${existing.number} for ${budget.org}.`);
}

async function upsertIncident({ github, core, env = process.env, now = Date.now() }) {
  if (env.BUDGET_MONITOR_ARMED !== 'true') {
    core.warning('Actions budget monitor is disarmed (BUDGET_MONITOR_ARMED != true); skipping incident upsert.');
    return;
  }

  const [homeOwner, homeRepo] = (env.GITHUB_REPOSITORY || '').split('/');
  const issueAuthorLogin = env.ISSUE_AUTHOR_LOGIN;
  if (!homeOwner || !homeRepo) {
    throw new Error('GITHUB_REPOSITORY must be set to the owner/repo of the monitor workflow.');
  }
  if (!issueAuthorLogin) {
    throw new Error('ISSUE_AUTHOR_LOGIN is required to restrict incident-issue adoption to this workflow\'s own identity.');
  }
  const budget = parseBudget(env.BUDGET_JSON);
  if (!budget.org) {
    throw new Error('BUDGET_JSON must carry the organization it measured.');
  }
  const nowIso = new Date(now).toISOString();

  await upsertPoolIncident({ github, core, homeOwner, homeRepo, issueAuthorLogin, budget, nowIso });
}

module.exports = {
  STANDARD_HOSTED_SKUS,
  billingMonth,
  fetchUsage,
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
  summarizeUsage,
  buildPoolIncidentBody,
  reportedThresholdsForMonth,
  tierMarker,
  upsertIncident,
};
