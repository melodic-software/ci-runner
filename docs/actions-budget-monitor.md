# Actions budget monitor

The public repository runs a hosted, alert-only monitor once a day that reports
how much of the organization's included GitHub Actions minute allowance the
current billing month has consumed, and whether any usage was billed at all.
Public standard hosted execution is free, so the monitor does not draw on the
allowance it watches.

Included-minute consumption is a month-scale signal, so this monitor runs daily
rather than at the queue monitor's quarter-hourly cadence.

## Alert channel

Detection upserts a marker-deduped incident issue in this repository through the
same channel, adoption rules, and escaping the queue monitor uses; that behavior
and its rationale are documented once, in
[Managed runner queue monitor](queue-monitor.md). This monitor keys two
independent incidents, because allowance pressure and billed spend have
different remediations and clear independently:

| Condition | Title | Marker |
| --- | --- | --- |
| 50% or 80% of the included allowance consumed | `[Alert] Actions included-minute consumption — <org>` | `<!-- ci-runner:actions-budget-monitor:incident:<org>:pool -->` |
| Any billed (net) usage in the month | `[Alert] Actions billed spend under a $0 posture — <org>` | `<!-- ci-runner:actions-budget-monitor:incident:<org>:net-spend -->` |

A repeat detection at the same threshold silently updates the incident body, as
in the queue monitor. Crossing from 50% to 80% instead adds a comment: an edited
body notifies nobody, so an escalation would otherwise be invisible. Which
thresholds an open incident has already reported is recorded in its body as
`<!-- ci-runner:actions-budget-monitor:tier:<percent> -->` markers, so the
comment fires exactly once per threshold. Tuning the boundary behavior further —
hysteresis policy — is deliberately out of scope here.

A new billing month resets consumption, which closes both incidents through the
ordinary recovery path.

## Billing data stays out of this public repository

The alert reports consumed minutes, percentage of the allowance, the affected
SKUs, and the bare fact that billed usage occurred. It never publishes an
amount, a price, or a per-repository cost — the only currency token in an alert
body is the `$0` spending-limit posture the alert is named after. This is the
standing constraint recorded under
[Independent monitor and cost evidence](roadmap.md#independent-monitor-and-cost-evidence);
a test asserts it against the rendered bodies rather than leaving it to review.

## Configuration

| Name | Kind | Meaning |
| --- | --- | --- |
| `CI_RUNNER_BILLING_OBSERVER_TOKEN` | secret | Token that may read organization billing usage |
| `INCLUDED_MINUTES` | workflow `env` | The plan's monthly included-minute allowance |

The measured organization is this repository's own owner; no variable declares
it. `INCLUDED_MINUTES` is workflow configuration rather than a constant because
the allowance is plan-dependent: GitHub documents 2,000 minutes per month for
GitHub Free and GitHub Free for organizations, 3,000 for GitHub Pro and GitHub
Team, and 50,000 for GitHub Enterprise Cloud. See
[GitHub Actions billing](https://docs.github.com/en/billing/concepts/product-billing/github-actions).

`CI_RUNNER_BILLING_OBSERVER_TOKEN` is read as a named secret and nothing more.
Deciding which credential mints or extends billing-usage read scope — a new
credential, or an extension of the existing observer App — is a secret-binding
change that goes through its own review; this workflow does not presume it.
Until that secret is provisioned the workflow fails loudly on every run, by
design: an unprovisioned credential must never be indistinguishable from a month
with no consumption, which is precisely the reading that would suppress the
alert. The workflow passes only the secret's presence (a boolean) into the
script, never its value.

## How consumption is measured

The monitor reads the enhanced billing platform's usage report,
[`GET /organizations/{org}/settings/billing/usage`](https://docs.github.com/en/rest/billing/usage),
scoped to the current UTC billing month, and sums — per SKU — the minutes the
included allowance actually paid for: each Actions minute item's
`discountAmount` divided by its `pricePerUnit`.

That "discounted quantity" is the right numerator rather than the raw
`quantity`, because `quantity` also counts minutes that never drew on the
allowance:

- Usage billed after the allowance ran out carries no discount.
- Usage that was never chargeable carries no discount either — GitHub bills no
  minutes for public repositories on standard hosted runners, and none for
  self-hosted runners.

Actions usage reported under a unit type the monitor does not recognize as
minutes fails the run rather than resolving to zero consumption, so a change in
the usage report's vocabulary cannot silently read as a healthy month.

### On runner minute multipliers

This calculation applies no per-runner minute multiplier. GitHub's current
Actions billing documentation expresses runner cost differences only as
per-minute prices, and states no multiplier against the included allowance;
the historical Windows 2x and macOS 10x factors do not appear in it. Applying
them on top of prices that already differ by runner type would over-report
consumption and alert early.

This is the one point in the calculation that could not be confirmed by a
positive statement in GitHub's documentation — only by its absence. The
incident body therefore breaks consumption out per SKU, so a reader can see the
composition and re-derive the total if GitHub's allowance semantics are ever
documented differently.

## Schedule availability boundary

The same scheduler caveats the queue monitor documents apply here, including
GitHub's automatic disabling of schedules in a public repository after 60 days
without repository activity: see
[Schedule availability boundary](queue-monitor.md#schedule-availability-boundary).
A daily monitor tolerates a delayed or dropped scheduled run more comfortably
than a queue watchdog does, because month-scale consumption changes slowly, but
no elapsed-time guarantee is claimed while the workflow is disabled.
