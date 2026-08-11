# Actions budget monitor

The public repository runs a hosted, alert-only monitor once a day that reports
how much of the organization's included GitHub Actions minute allowance the
current billing month has consumed on private repositories. Public standard
hosted execution is free, so the monitor does not draw on the allowance it
watches.

Included-minute consumption is a month-scale signal, so this monitor runs daily
rather than at the queue monitor's quarter-hourly cadence.

## Arming

The monitor is gated on the repository variable
`CI_RUNNER_BUDGET_MONITOR_ARMED == 'true'`. When disarmed, the workflow emits a
warning and skips measurement and incident upsert. When armed but
`CI_RUNNER_BILLING_OBSERVER_TOKEN` is not yet provisioned, the workflow fails
loudly — an unprovisioned credential must never read as a healthy zero-consumption
month. Deciding which credential mints or extends billing-usage read scope stays
in [#113](https://github.com/melodic-software/ci-runner/issues/113); this
workflow does not mint credentials.

## Alert channel

Detection upserts a marker-deduped incident issue in this repository through the
same channel, adoption rules, and escaping the queue monitor uses; that behavior
and its rationale are documented once, in
[Managed runner queue monitor](queue-monitor.md).

| Condition | Title | Marker |
| --- | --- | --- |
| 50% or 80% of the included allowance consumed | `[Alert] Actions included-minute consumption — <org>` | `<!-- ci-runner:actions-budget-monitor:incident:<org>:pool -->` |

A repeat detection at the same threshold silently updates the incident body, as
in the queue monitor. Crossing from 50% to 80% instead adds a comment: an edited
body notifies nobody, so an escalation would otherwise be invisible. Which
thresholds an open incident has already reported is recorded in its body as
`<!-- ci-runner:actions-budget-monitor:tier:<percent> -->` markers, so the
comment fires exactly once per threshold.

A new billing month resets consumption, which closes the incident through the
ordinary recovery path.

## Billing data stays out of this public repository

The alert reports consumed minutes, percentage of the allowance, and the
affected SKUs. It never publishes an amount, a price, or a per-repository cost.
This is the standing constraint recorded under
[Independent monitor and cost evidence](roadmap.md#independent-monitor-and-cost-evidence);
a test asserts it against the rendered bodies rather than leaving it to review.

## Configuration

| Name | Kind | Meaning |
| --- | --- | --- |
| `CI_RUNNER_BUDGET_MONITOR_ARMED` | variable | When `'true'`, run measurement and upsert; otherwise skip with a warning |
| `CI_RUNNER_BILLING_OBSERVER_TOKEN` | secret | Token that may read organization billing usage and resolve repository visibility |
| `INCLUDED_MINUTES` | workflow `env` | The plan's monthly included-minute allowance (3,000 for GitHub Team) |

The measured organization is this repository's own owner; no variable declares
it. `INCLUDED_MINUTES` is workflow configuration rather than a constant because
the allowance is plan-dependent: GitHub documents 2,000 minutes per month for
GitHub Free and GitHub Free for organizations, 3,000 for GitHub Pro and GitHub
Team, and 50,000 for GitHub Enterprise Cloud. See
[GitHub Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).

## How consumption is measured

The monitor reads the enhanced billing platform's usage report,
[`GET /organizations/{org}/settings/billing/usage`](https://docs.github.com/en/rest/billing/usage),
scoped to the current UTC billing month.

For each usage row it:

1. Keeps only `product == actions`, `unitType == Minutes`, and the standard
   hosted SKUs (`Actions Linux`, `Actions Linux Slim`, `Actions Windows`).
   Larger runners, self-hosted labels, and non-minute Actions products are
   excluded.
2. Drops rows missing required fields (`organizationName`, `repositoryName`,
   `sku`, `quantity`).
3. Resolves repository visibility via `GET /repos/{owner}/{repo}`. Rows on
   **private** repositories contribute their raw `quantity`. Rows on **public**
   repositories contribute nothing. When visibility is unknown (404, permission
   error, or any lookup failure), the row's minutes are **included** rather than
   dropped — under-counting would suppress alerts; over-counting is the safer
   failure mode for a budget watchdog.
4. Sums minutes per SKU and compares the total against `INCLUDED_MINUTES` at
   50% and 80%.

No runner minute multiplier is applied. No `discountAmount` / `pricePerUnit`
ratio is used; the numerator is raw `quantity` on eligible private-repo rows.

Actions usage reported under a unit type the monitor does not recognize as
minutes fails the run rather than resolving to zero consumption, so a change in
the usage report's vocabulary cannot silently read as a healthy month.

## Schedule availability boundary

The same scheduler caveats the queue monitor documents apply here, including
GitHub's automatic disabling of schedules in a public repository after 60 days
without repository activity: see
[Schedule availability boundary](queue-monitor.md#schedule-availability-boundary).
A daily monitor tolerates a delayed or dropped scheduled run more comfortably
than a queue watchdog does, because month-scale consumption changes slowly, but
no elapsed-time guarantee is claimed while the workflow is disabled.
