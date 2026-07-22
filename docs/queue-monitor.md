# Managed runner queue monitor

The public repository runs a hosted, alert-only monitor at minutes 7, 22, 37,
and 52 of every hour. Public standard hosted execution is free, and keeping this
control plane off the managed fleet means it still reports when both local hosts
are unavailable.

The monitor mints a short-lived installation token, scoped to only repository
Actions read permission on the monitored repositories, to inspect queue depth.
Writing the incident issue in this repository uses the job's own default
`GITHUB_TOKEN` instead, so no additional IaC configuration is needed for
alerting. Configure these values through IaC:

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
treated as runner-capacity failures.

A successful execution reports green regardless of what it finds: a queued job
older than five minutes is a capacity alert, not a monitor failure, so it no
longer fails the run. By-design reds here used to pollute fleet-wide failure
dashboards and, since scheduled runs have no actor, reached nobody who wasn't
watching the Actions tab. Detection instead upserts a marker-deduped incident
issue in this repository — the fleet's established alert-per-incident pattern
(see `link-check.yml`, `queue-monitor-liveness.yml`, and
`standards-sync-stuck-automerge-alert.yml` in `melodic-software/ci-workflows`):
one open issue per target owner, titled
`[Alert] Managed runner queue capacity — <owner>` and carrying a hidden
`<!-- ci-runner:queued-job-monitor:incident:<owner> -->` marker in its body,
silently updated in place on repeat detections (an edited issue body notifies
nobody, unlike a comment) and closed with a recovery comment once the queue
clears. Because this repository is public, the marker and title text are
themselves public (embedded verbatim in this workflow's source), so adoption
is additionally restricted to issues authored by this workflow's own
`GITHUB_TOKEN` identity (`github-actions[bot]`) — otherwise a non-maintainer
could open a decoy issue that gets silently adopted, updated, or closed as
recovered, suppressing a real alert. Matching stays marker-only, matching the
fleet precedent's deliberate "a marker survives a retitle" property: the
detection table embeds monitored-repo job and workflow names verbatim, so a
crafted job name could otherwise inject a different owner's marker string
into a bot-authored body and cause a cross-owner collision — the table
neutralizes `<` and `>` at render time (HTML-entity-encoded, so they still
display literally) rather than layering a second, title-based guard that
would trade away retitle-survival for redundant protection. More than one
own-authored issue carrying the same marker fails the run closed rather than
guessing which one is authoritative. The job's own `GITHUB_TOKEN` (job-level
`issues: write`)
writes that issue, separately from the read-only, target-scoped observer
token used to inspect queued jobs, which cannot write here. Normal GitHub
notifications on issue creation answer the "no actor" constraint. The issue
body carries the detection table, the capacity window (`constrained since`
the issue's creation timestamp), and the affected queue depth, so no-runner
failures elsewhere can be cross-referenced against it. The table caps at 50
rows with a "…and N more, see the workflow run" remainder note linking back
to the run, and the whole body is bounded well under GitHub's issue-body
write limit (empirically 65536 characters) while always preserving the
trailing marker intact, so a queue-wide outage with many stuck jobs cannot
itself break the alert. Only a genuine execution error — a bad configuration,
a GitHub API failure, an ambiguous marker match — still fails the run; that
is the monitor breaking, not a queue alert.

Each alert links directly to the affected jobs and carries this recovery
instruction:

> Follow the audited CI routing-control procedure to make the affected
> repository's effective `CI_RUNNER_POLICY` value `hosted-only` and verify the
> readback. Cancel the affected run, choose **Re-run all jobs** to guarantee
> that the selector executes again, and confirm that it selects hosted capacity.
> Do not use a failed-job or single-job rerun for this recovery because
> partial-rerun dependency behavior does not guarantee a fresh selector
> decision. A `workflow_dispatch` creates a separate run with different event
> and ref context; it does not recover the original pull-request check.

The monitor never changes policy, cancels, reruns, dispatches, or mutates a
workload. The central selector applies its policy-driven routing rules on every
attempt; it has no rerun-only hosted branch. Because a repository variable
overrides an organization variable with the same name, recovery verifies the
effective value instead of assuming an organization update is sufficient. The
canonical procedure is the
[`github-iac` local-CI routing runbook](https://github.com/melodic-software/github-iac/blob/main/README.md#local-ci-routing-governance);
GitHub documents the [full and partial rerun
operations](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs),
the [`workflow_dispatch` event
context](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#workflow_dispatch),
and the precedence rule in its [variables
reference](https://docs.github.com/en/actions/reference/workflows-and-actions/variables#configuration-variable-precedence).

Watch this repository's issues (or the `[Alert] Managed runner queue capacity`
title prefix) rather than the failed-workflow email GitHub sends for the
account that owns the schedule: a healthy detection run is green and sends no
such notification. See [workflow-run
notifications](https://docs.github.com/en/actions/concepts/workflows-and-actions/notifications-for-workflow-runs)
for the (now unused, by design) failure-notification path.

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
