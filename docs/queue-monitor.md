# Managed runner queue monitor

The public repository runs a hosted, alert-only monitor at minutes 7, 22, 37,
and 52 of every hour. Public standard hosted execution is free, and keeping this
control plane off the managed fleet means it still reports when both local hosts
are unavailable.

The monitor mints a short-lived installation token with only repository Actions
read permission. Configure these values through IaC:

| Name | Kind | Meaning |
| --- | --- | --- |
| `CI_RUNNER_OBSERVER_CLIENT_ID` | variable | Observer GitHub App client ID |
| `CI_RUNNER_OBSERVER_PRIVATE_KEY` | secret | Observer App private key |
| `CI_RUNNER_MONITOR_TARGETS_JSON` | variable | Installation-target JSON array |

`CI_RUNNER_OBSERVER_CLIENT_ID` deliberately uses the GitHub App client ID.
`actions/create-github-app-token` deprecates its numeric `app-id` input; the
numeric `CI_RUNNER_OBSERVER_APP_ID` name is therefore rejected rather than kept
as an alias.

Each target contains `owner`, comma/newline-separated `repositories`, and
comma/newline-separated exact `managedLabels`. The organization-only value is:

```json
[
  {
    "owner": "melodic-software",
    "repositories": "medley,standards,claude-code-plugins,github-iac",
    "managedLabels": "melodic-ubuntu-24.04-x64"
  }
]
```

An installation token belongs to exactly one GitHub App installation. The
personal phase therefore appends a `kyle-sexton` object instead of changing the
workflow. Each matrix job mints an independent Actions-read token for its owner.
Repository lists remain data owned by IaC; the example documents shape, not a
hard-coded runtime inventory.

For each configured repository the monitor paginates every nonterminal workflow
run status GitHub exposes (`queued`, `in_progress`, `requested`, `waiting`, and
`pending`), deduplicates runs, paginates their latest jobs, and finds
runner-eligible `queued` jobs carrying an exact managed label. Jobs still
`requested`, `waiting` on dependencies, or `pending` behind concurrency are not
treated as runner-capacity failures. A queued job older than five minutes fails
the monitor with direct links and this recovery instruction:

> Cancel the affected run, then choose “Re-run all jobs.”

The monitor never cancels, reruns, dispatches, or mutates a workload. A full
rerun has `github.run_attempt > 1`, so the central selector chooses hosted
capacity. This is the explicit recovery contract for the irreducible case where
both local hosts disappear after local selection.

Enable GitHub Actions failed-workflow email or web notifications for the account
that owns the schedule. GitHub sends scheduled-workflow notifications to the
user associated with the cron workflow, subject to that user's notification
settings. See [workflow-run notifications](https://docs.github.com/en/actions/concepts/workflows-and-actions/notifications-for-workflow-runs).

## Schedule availability boundary

The four off-the-hour entries reduce contention; they do not form a hard
15-minute watchdog. GitHub documents that scheduled events can be delayed during
high load and, under sufficiently high load, some queued scheduled jobs can be
dropped. GitHub also automatically disables schedules in a public repository
after 60 days without repository activity. See [scheduled-event
behavior](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule)
and [automatic schedule disabling](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/disable-and-enable-workflows).

Accordingly, the five-minute queue threshold applies when a monitor invocation
runs; no elapsed-time SLO is claimed while GitHub's scheduler is delayed or the
workflow is disabled. The production rollout checklist must verify that this
workflow is enabled and has a recent successful run. A future independent
control-plane check that alerts on a disabled/stale monitor is an explicit
roadmap item; it must not depend on this same schedule.
