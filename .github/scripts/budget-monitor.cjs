'use strict';

const {
  boundBodyLength,
  escapeMarkdownTableCell,
  findOpenIncident,
} = require('./incident-issue.cjs');

// Percentages of the included-minute allowance that open a pool incident. Both
// stay breached once crossed within a month (consumption only rises until the
// allowance resets), so the body reports the highest tier reached and the
// escalation between them is a comment, not a second issue. Tuning the
// boundary behavior beyond this is the hysteresis policy that stays in #113.
const POOL_ALERT_THRESHOLD_PERCENTS = Object.freeze([50, 80]);

// The usage report's own product/unit vocabulary. Matched case-insensitively
// because GitHub documents the field names but not the value casing, and a
// casing change must not silently reclassify Actions minutes as "no usage".
const ACTIONS_PRODUCT = 'actions';
const MINUTE_UNIT_TYPES = Object.freeze(['minutes', 'minute']);

// A worst-case month could bill many distinct SKUs; capping the rendered table
// keeps the incident body bounded regardless of how many appear.
const MAX_SKU_TABLE_ROWS = 25;

function isActionsMinuteItem(item) {
  return String(item.product ?? '').toLowerCase() === ACTIONS_PRODUCT
    && MINUTE_UNIT_TYPES.includes(String(item.unitType ?? '').toLowerCase());
}

function isActionsItem(item) {
  return String(item.product ?? '').toLowerCase() === ACTIONS_PRODUCT;
}

// The share of an item's minutes that the included allowance paid for.
// `discountAmount` is that item's discount in currency and `pricePerUnit` its
// currency-per-minute, so their quotient is minutes — the "discounted quantity"
// the allowance actually absorbed, rather than `quantity`, which also counts
// minutes billed after the allowance ran out and minutes that were never
// chargeable at all (GitHub bills no minutes for public repositories on
// standard runners, so those carry no discount and correctly consume nothing).
//
// Deliberately multiplier-free: GitHub's current Actions billing documentation
// expresses runner cost differences only as per-minute prices and states no
// multiplier against the included allowance, so applying the historical
// Windows 2x / macOS 10x factors here would over-report consumption. Per-SKU
// minutes are rendered in the incident body so a reader can see the
// composition if that ever needs revisiting.
function discountedMinutes(item) {
  const pricePerUnit = Number(item.pricePerUnit);
  const discountAmount = Number(item.discountAmount);
  if (!Number.isFinite(pricePerUnit) || pricePerUnit <= 0) return 0;
  if (!Number.isFinite(discountAmount) || discountAmount <= 0) return 0;
  return discountAmount / pricePerUnit;
}

function roundMinutes(minutes) {
  return Math.round(minutes * 10) / 10;
}

// Reports the UTC billing month. The usage endpoint defaults to the current
// year and month, but the reporting month is also what keys the incident body
// and what makes a rollover observable, so it is resolved here rather than
// left implicit in the API's default.
function billingMonth(now) {
  const date = new Date(now);
  return { year: date.getUTCFullYear(), month: date.getUTCMonth() + 1 };
}

function formatBillingMonth({ year, month }) {
  return `${year}-${String(month).padStart(2, '0')}`;
}

// Currency never leaves this function. The incident issue lives in a public
// repository, so the alert reports consumed minutes, percentage of the
// allowance, and the bare fact that billed spend occurred — never an amount,
// a price, or a per-repository cost. Keeping billing figures out of this
// repository is a standing constraint (see docs/roadmap.md, "Independent
// monitor and cost evidence").
function summarizeUsage(usageItems, { includedMinutes }) {
  if (!Array.isArray(usageItems)) {
    throw new Error('The billing usage report must expose a usageItems array.');
  }
  if (!Number.isFinite(includedMinutes) || includedMinutes <= 0) {
    throw new Error('Included minutes must be a positive number.');
  }

  const actionsItems = usageItems.filter(isActionsItem);
  const minuteItems = actionsItems.filter(isActionsMinuteItem);
  // Actions usage that reports no recognizable minute unit is a vocabulary
  // change, not a quiet zero: reporting 0% consumed would read as a healthy
  // month and suppress the alert this workflow exists to raise.
  if (actionsItems.length > 0 && minuteItems.length === 0) {
    throw new Error('Actions usage was reported but no item carried a minute unit type; the usage-report vocabulary changed.');
  }

  const minutesBySku = new Map();
  for (const item of minuteItems) {
    const minutes = discountedMinutes(item);
    if (minutes <= 0) continue;
    const sku = String(item.sku ?? 'unknown');
    minutesBySku.set(sku, (minutesBySku.get(sku) || 0) + minutes);
  }

  const consumedMinutes = [...minutesBySku.values()].reduce((total, minutes) => total + minutes, 0);
  const percentUsed = (consumedMinutes / includedMinutes) * 100;

  const netSpendProducts = [...new Set(
    usageItems
      .filter(item => Number(item.netAmount) > 0)
      .map(item => String(item.product ?? 'unknown')),
  )].sort();

  return {
    includedMinutes,
    consumedMinutes: roundMinutes(consumedMinutes),
    percentUsed: Math.round(percentUsed * 10) / 10,
    breachedThresholds: POOL_ALERT_THRESHOLD_PERCENTS.filter(threshold => percentUsed >= threshold),
    netSpendProducts,
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

  let summary;
  let month;
  try {
    if (!org) {
      throw new Error('BILLING_ORG is required to scope the billing usage report.');
    }
    // An unprovisioned secret must never read as a zero-consumption month.
    // github-script hides the token inside its Octokit client, so the workflow
    // asserts presence and this refuses to measure without it.
    if (env.BILLING_TOKEN_PRESENT !== 'true') {
      throw new Error('The billing observer token is not provisioned: set the CI_RUNNER_BILLING_OBSERVER_TOKEN secret. Refusing to report consumption without it.');
    }
    month = billingMonth(now);
    summary = summarizeUsage(await fetchUsage({ github, org, ...month }), { includedMinutes });
  } catch (error) {
    core.setFailed(error instanceof Error ? error.message : String(error));
    return;
  }

  const budget = { org, month: formatBillingMonth(month), ...summary };

  // Handed to the incident-upsert step via job output; a successful execution
  // stays green from here regardless of what it found (see upsertIncident).
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

function netSpendIncidentTitle(org) {
  return `[Alert] Actions billed spend under a $0 posture — ${org}`;
}

function netSpendIncidentMarker(org) {
  return `<!-- ci-runner:actions-budget-monitor:incident:${org}:net-spend -->`;
}

// Records which thresholds an open pool incident has already reported. A body
// edit notifies nobody, so without this a 50% incident escalating to 80% would
// change silently; comparing these markers turns a genuine tier change into
// the one comment that does notify, while a repeat detection at the same tier
// stays a silent body update.
function tierMarker(threshold) {
  return `<!-- ci-runner:actions-budget-monitor:tier:${threshold} -->`;
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

const netSpendRecoverySummary = `
Billed spend is impossible while the organization's spending limit is \`$0\`, so a non-zero net amount means the limit itself has drifted. Verify the organization's Actions spending limit through the audited billing-settings procedure before changing any workload. This alert deliberately reports no amounts: this repository is public and billing figures stay out of it.
`;

function parseBudget(budgetJson) {
  // A missing or empty BUDGET_JSON is never "no consumption" — the measurement
  // step always emits an explicit object via core.setOutput. Falling back to a
  // healthy default here would silently treat a wiring bug or a skipped step
  // as a recovery and close a real open incident.
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
  if (!Array.isArray(budget.breachedThresholds) || !Array.isArray(budget.netSpendProducts)) {
    throw new Error('BUDGET_JSON must carry breachedThresholds and netSpendProducts arrays.');
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
  const tierMarkers = budget.breachedThresholds.map(tierMarker).join('');
  const escalatedThresholds = existing
    ? budget.breachedThresholds.filter(threshold => !(existing.body ?? '').includes(tierMarker(threshold)))
    : [];

  const bodyWithoutMarker = [
    // Tier markers lead the body so the length bound, which truncates from the
    // end, can never drop the escalation state this comparison depends on.
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
  const body = boundBodyLength(bodyWithoutMarker, marker);

  if (!existing) {
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

  // A silent body update, not a comment: this step runs daily while an
  // incident stays open, and GitHub notifies watchers on every comment but not
  // on a body edit. The body's "last confirmed" timestamp carries freshness.
  await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body });
  if (escalatedThresholds.length > 0) {
    await github.rest.issues.createComment({
      owner: homeOwner,
      repo: homeRepo,
      issue_number: existing.number,
      body: `Escalated to ${highestThreshold}% of the included allowance: ${budget.consumedMinutes} of ${budget.includedMinutes} minute(s) consumed in ${budget.month} as of ${nowIso}.`,
    });
  }
  core.info(`Updated pool incident #${existing.number} for ${budget.org}.`);
}

async function upsertNetSpendIncident({ github, core, homeOwner, homeRepo, issueAuthorLogin, budget, nowIso }) {
  const marker = netSpendIncidentMarker(budget.org);
  const existing = await findOpenIncident({ github, homeOwner, homeRepo, marker, issueAuthorLogin });

  if (budget.netSpendProducts.length === 0) {
    if (!existing) {
      core.info(`No open net-spend incident for ${budget.org}; nothing was billed.`);
      return;
    }
    await closeIncident({
      github,
      homeOwner,
      homeRepo,
      core,
      existing,
      recoveryBody: `Recovered: no billed usage is reported for \`${budget.org}\` in ${budget.month} as of ${nowIso}.`,
    });
    return;
  }

  const products = budget.netSpendProducts.map(product => `\`${escapeMarkdownTableCell(product)}\``).join(', ');
  const bodyWithoutMarker = [
    `Billed usage is reported for \`${budget.org}\` in ${budget.month}, which the organization's \`$0\` spending limit should make impossible.`,
    '',
    `Affected product(s): ${products}. Last confirmed ${nowIso}.`,
    '',
    netSpendRecoverySummary.trim(),
  ].join('\n');
  const body = boundBodyLength(bodyWithoutMarker, marker);

  if (existing) {
    await github.rest.issues.update({ owner: homeOwner, repo: homeRepo, issue_number: existing.number, body });
    core.info(`Updated net-spend incident #${existing.number} for ${budget.org}.`);
    return;
  }
  const created = await github.rest.issues.create({
    owner: homeOwner,
    repo: homeRepo,
    title: netSpendIncidentTitle(budget.org),
    body,
    labels: ['automated'],
  });
  core.info(`Opened net-spend incident #${created.data.number} for ${budget.org}.`);
}

// Marker-deduped issue-per-incident alert channel, one incident per condition:
// allowance consumption and billed spend have different remediations and clear
// independently, so they never share an issue. Runs on the job's own
// GITHUB_TOKEN against this repository — distinct from the billing-scoped token
// the measurement step uses, which reads organization billing and has no
// business writing issues here. Any thrown error here fails the run: an
// incident-issue write failure is the monitor breaking, not a budget alert.
async function upsertIncident({ github, core, env = process.env, now = Date.now() }) {
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
  await upsertNetSpendIncident({ github, core, homeOwner, homeRepo, issueAuthorLogin, budget, nowIso });
}

module.exports = {
  billingMonth,
  discountedMinutes,
  fetchUsage,
  formatBillingMonth,
  MAX_SKU_TABLE_ROWS,
  netSpendIncidentMarker,
  netSpendIncidentTitle,
  netSpendRecoverySummary,
  parseBudget,
  POOL_ALERT_THRESHOLD_PERCENTS,
  poolIncidentMarker,
  poolIncidentTitle,
  poolRecoverySummary,
  renderSkuMarkdownTable,
  run,
  summarizeUsage,
  tierMarker,
  upsertIncident,
};
