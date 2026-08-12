# OpenTelemetry observability

The controller has optional, first-class OTLP trace and metric export. Production
deployments should use the reviewed host YAML so endpoint, protocol, signal
enablement, and metric cadence travel with the rest of the fleet configuration:

```yaml
telemetry:
  endpoint: http://127.0.0.1:4317
  protocol: grpc
  traces: true
  metrics: true
  metricExportInterval: 15s
  metricExportTimeout: 10s
```

`endpoint` is the common OTLP base URL. For `http/protobuf`, the controller
appends the standard `/v1/traces` and `/v1/metrics` signal paths; for `grpc`, it
uses the URL directly.

Omitting the block preserves the standard OpenTelemetry environment-variable
interface for ad hoc and legacy deployments. When the block is present, its
signal selection, endpoint, protocol, and metric cadence take precedence over
ambient process environment. Exporter-specific standard variables, such as
headers and certificates, remain available for secrets and transport details.

Telemetry is fully disabled when both the YAML block and environment are unset. In that state the
controller does not create an exporter or contact the OpenTelemetry localhost
defaults. It becomes enabled per signal when either its exporter is explicitly
`otlp`, its signal-specific endpoint is set, or the common OTLP endpoint is
set. An explicit signal exporter of `none` wins. `OTEL_SDK_DISABLED=true`
disables both signals.

This example exports traces and metrics over OTLP/HTTP to a local collector:

```text
OTEL_TRACES_EXPORTER=otlp
OTEL_METRICS_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
OTEL_METRIC_EXPORT_INTERVAL=15000
OTEL_METRIC_EXPORT_TIMEOUT=10000
```

For OTLP/gRPC, set `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` and use the collector's
gRPC endpoint, commonly `http://127.0.0.1:4317`. Signal-specific protocol,
endpoint, header, certificate, timeout, and compression variables supported by
the Go OTLP exporters take precedence over their common forms. In particular,
`OTEL_EXPORTER_OTLP_TRACES_*` and `OTEL_EXPORTER_OTLP_METRICS_*` can route the
signals independently. Collector credentials in `OTEL_EXPORTER_OTLP_HEADERS`
are consumed only by the exporter; the controller never copies them into logs,
state, spans, metrics, or worker containers.

The scheduled controller must be gracefully restarted after changing its
configuration or environment. Telemetry initialization, collection, export, and bounded
shutdown are outside the admission and worker lifecycle decision paths.
Configuration and initialization problems are written as
`telemetry-configuration-error`; asynchronous exporter failures are written as
`telemetry-export-error`. Neither condition advertises zero, drains a worker,
cancels a job, or terminates the controller.

## Resource identity

Every exported signal has a fixed, nonsecret resource identity:

- `service.name=ci-runner-controller`
- `service.namespace=melodic-software`
- `service.version` equal to the immutable controller version
- `service.instance.id` and `host.name` equal to the provisioning-owned host ID

GitHub App client and installation IDs, secret IDs, repository URLs, scale-set
listener IDs, runner names, runner IDs, container IDs, and job IDs are never
resource or metric attributes.

## Traces

Each serialized controller step emits one `controller.reconcile` internal span.
It reports only the bounded reconcile result and final controller phase. An
ordinary process-shutdown cancellation is classified as `canceled` and does
not set error status. Other reconciliation failures set error status and
increment the reconcile error counter.

## Metrics

All metrics use the `ci_runner` namespace. Gauges represent the most recent
completed reconciliation, except `ci_runner.capacity.acknowledged` and
`ci_runner.capacity.acknowledgement.pending.age`, which are also refreshed from
durable poll-cadence checkpoints during open listener polls; counters are
monotonic process-lifetime events.

| Metric | Meaning | Attributes |
| --- | --- | --- |
| `ci_runner.controller.reconcile.duration` | Reconcile duration in seconds | `ci_runner.reconcile.result` |
| `ci_runner.controller.reconcile.errors` | Unexpected reconcile failures | none |
| `ci_runner.controller.observed.checkpoint.age` | Prior durable checkpoint age at reconcile start; omitted when missing, corrupt, or future-dated | none |
| `ci_runner.capacity.advertised` | Capacity acknowledged to GitHub | `ci_runner.pool.id` |
| `ci_runner.capacity.acknowledged` | Latest target capacity is listener-acknowledged, `0` or `1`; refreshed from durable poll-cadence checkpoints during open listener polls | `ci_runner.pool.id` |
| `ci_runner.capacity.acknowledgement.pending.age` | Age of a pending capacity transition; omitted when acknowledged or unavailable; refreshed from durable poll-cadence checkpoints during open listener polls | `ci_runner.pool.id` |
| `ci_runner.capacity.assigned` | Authoritative assigned jobs | `ci_runner.pool.id` |
| `ci_runner.capacity.desired` | Desired local workers | `ci_runner.pool.id` |
| `ci_runner.workers` | Workers in each bounded state | `ci_runner.pool.id`, `ci_runner.worker.state` |
| `ci_runner.jobs.active` | Busy workers/active jobs | `ci_runner.pool.id` |
| `ci_runner.docker.inventory.workers` | Reconciled Docker inventory count | `ci_runner.pool.id` |
| `ci_runner.accounting.assignment.gap` | Assigned minus visible busy/starting workers | `ci_runner.pool.id` |
| `ci_runner.accounting.transient_lag` | Bounded short-job lag classification, `0` or `1` | `ci_runner.pool.id` |
| `ci_runner.host.cpu.utilization` | Host CPU utilization percent | none |
| `ci_runner.host.memory.available` | Available physical memory in bytes | none |
| `ci_runner.capacity.memory.headroom` | Memory left unspent by the last plan under the active basis (static worker budget, or legacy host headroom) | none |
| `ci_runner.capacity.memory.affordable` | Additional workers the remaining memory headroom funds at the pool's effective worker profile | `ci_runner.pool.id` |
| `ci_runner.gate.resource.blocked` | Resource gate, `0` or `1` | none |
| `ci_runner.gate.power.blocked` | Power gate, `0` or `1` | none |
| `ci_runner.worker.starts` | Worker start attempts | `ci_runner.pool.id`, bounded outcome |
| `ci_runner.worker.start.duration` | Docker worker start duration | pool, tier, bounded outcome |
| `ci_runner.worker.registrations` | GitHub JIT registrations | pool, tier, bounded outcome |
| `ci_runner.worker.registration.duration` | JIT registration duration | pool, tier, bounded outcome |
| `ci_runner.worker.finalizations` | Container finalizations | `ci_runner.pool.id`, bounded outcome |
| `ci_runner.worker.finalization.duration` | Artifact/container finalization duration | pool, tier, bounded outcome |
| `ci_runner.worker.lifecycle.event.time` | Exact lifecycle event Unix time | pool, tier, bounded event/outcome |
| `ci_runner.jobs.started` | Durably indexed job-start events | `ci_runner.pool.id` |
| `ci_runner.jobs.start.visibility_lag` | Runner assignment to indexed start lag | `ci_runner.pool.id` |
| `ci_runner.jobs.completed` | Validated GitHub completion events | `ci_runner.pool.id`, bounded result |
| `ci_runner.cancellations` | Expected cancellations | bounded source and classification |
| `ci_runner.worker.resource.evidence` | Terminal cgroup evidence availability | pool, tier, bounded resource outcome |
| `ci_runner.worker.memory.peak` | Terminal `memory.peak` bytes | pool, tier, finalization outcome |
| `ci_runner.worker.memory.swap.peak` | Terminal `memory.swap.peak` bytes | pool, tier, finalization outcome |
| `ci_runner.worker.memory.oom.events` | Terminal OOM event count | pool, tier, finalization outcome |
| `ci_runner.worker.memory.oom_kill.events` | Terminal OOM-kill count | pool, tier, finalization outcome |
| `ci_runner.worker.cpu.periods` | Terminal CPU periods | pool, tier, finalization outcome |
| `ci_runner.worker.cpu.throttled.periods` | Terminal throttled CPU periods | pool, tier, finalization outcome |
| `ci_runner.worker.cpu.throttled.duration` | Terminal throttled CPU seconds | pool, tier, finalization outcome |
| `ci_runner.worker.pids.peak` | Terminal process peak | pool, tier, finalization outcome |
| `ci_runner.worker.io.read` | Terminal aggregate read bytes | pool, tier, finalization outcome |
| `ci_runner.worker.io.write` | Terminal aggregate write bytes | pool, tier, finalization outcome |

Pool IDs are stable configuration identifiers. Worker state, result, outcome,
source, and classification values come from closed vocabularies. Metrics never
contain a runner name, container ID, job ID, exception message, or arbitrary
error text.

Terminal resource histograms are emitted only for fields actually captured.
`partial`, `missing`, `unavailable`, and `invalid` evidence is counted
explicitly, so a missing cgroup or Docker archive API can never masquerade as a
zero peak. Each validated or fallback JSON record is an ACL-hardened sidecar
whose path is derived from the legacy diagnostic archive before the container
is eligible for removal. The schema-version-1 `jobs.json` shape remains
readable by v0.1.9 during rollback. A retained worker retry recognizes the
existing sidecar and does not emit terminal histograms or OOM counters twice.
Resource metrics use only the configured pool, the bounded
`default`/`target_override`/`unknown` tier, and bounded finalization/resource
outcomes—never a device number, runner name, container ID, or job ID.

If Docker reports that a worker disappeared between inventory and wait, the
controller records bounded `unknown` finalization and `missing` resource
evidence instead of `runtime_error` and `unavailable`. If the stopped container
still exists, its inspected exit code supplies the lifecycle outcome even when
the wait stream failed.

## Memory capacity basis and clamp visibility

Memory-affordable slot math runs on a static `resources.workerMemoryBudget`
when one is configured, otherwise on legacy host physical headroom. The two
gauges above report the basis outcome each reconcile: headroom is the unspent
remainder, and the per-pool affordable count is how many more workers that
remainder funds. When the memory term (or, under a budget, the host-floor
backstop at `minimumAvailableMemoryPercent`) binds worker starts or advertised
capacity below what host and pool limits allowed, the controller writes a
`memory-clamped-capacity` log line for the pool — previously this clamp was
silent. It is a log line, not a problem: the legacy basis clamps routinely
under load, and that alone must not report the fleet degraded.

Under a budget, each active worker is charged the memory limit the container
runtime reports it was started with, so headroom stays honest across a worker
profile change: workers started under the previous profile keep charging it
until they drain. Workers the runtime reports as unlimited are charged their
pool's effective profile instead, as are workers whose limit could not be read —
that read failure is surfaced as an adapter error, but it degrades the
reservation to the profile rather than failing the observation.

Two budget-specific signals are problems or warnings: a configured budget
larger than the probed engine VM memory raises the
`worker-memory-budget-exceeds-engine-memory` problem and clamps the effective
budget to the probe, and a failed probe writes `engine-memory-probe-error`
while the configured budget stays in effect unverified.

## Fast-job accounting freshness

`host status` is a durable point-in-time checkpoint, not an event ledger. A job
that registers, starts, and exits between reconciliations can be visible as busy
in GitHub while the last local checkpoint still contains zero workers, or can
already be finalized before the next checkpoint. Rewriting a finished worker
back into the current inventory would make drain and cleanup invariants false,
so the status model deliberately keeps its durable snapshot semantics.

The telemetry surface makes that timing explicit. Checkpoint age reports how
old the previous durable observation was when a reconcile began and is omitted
when the checkpoint is absent, corrupt, or future-dated. Docker
inventory and bounded worker-state gauges report the local point-in-time view.
For each pool, assignment gap compares authoritative GitHub assignments with
locally visible `busy` plus `starting` workers. A positive gap emits
`ci_runner.accounting.transient_lag=1` and adds the exact
`transientAccountingLag` classification to the reconcile span. It is timing
evidence, not by itself a frozen-runner error. Combine it with checkpoint age,
reconcile errors, lifecycle-event timestamps, and repeated gaps before alerting.

Capacity acknowledgement has its own transition signal. While a listener is
accepting a new resource-driven capacity, the pool transition timestamp remains
stable instead of resetting on each heartbeat. `host doctor` treats that state
as healthy for a bounded grace window, and a transition still pending after it
is unhealthy — a non-advisory fault that degrades the exit code, because a
listener that never acknowledges shows sustained, monotonically growing lag.
This reuses the existing schema-version-1 pool `updatedAt` field and does not
change the rollback-readable observed-state shape.

The window is derived from the configured retry policy. An acknowledgement
crosses two request paths — the controller advertising capacity, and a later
reconcile reading back the state that acknowledges it — and each is a complete
retry envelope: up to `github.retry.maxAttempts` attempts, each capped at
`github.requestTimeout`, separated by backoff waits of
`github.retry.maximum` × (1 + `github.retry.jitterRatio`), since jitter is
applied after the backoff base is capped. The window budgets that full envelope
per path, plus two reconciliation intervals.

The acknowledgement is not a protocol signal (the scale-set protocol never
acknowledges capacity back) but this controller's own convergence check, so the
window has to cover the request path convergence actually travels — and it has
to cover all of it, because nothing else in `host doctor` notices a poll that is
still legitimately retrying. While a poll is open the controller keeps writing
observed state on the reconciliation cadence, so the heartbeat stays fresh while
the pool transition timestamp stays deliberately pinned. Budgeting less than the
retry policy therefore hard-faults a listener whose configured retry sequence
has not finished, which is what previously made benign busy-fleet lag trip a
hard fault.

The cost is detection latency, which now scales with the retry policy; raising
`github.retry.maxAttempts` or `github.retry.maximum` widens this window too.
Detection itself is unaffected: a wedged listener never acknowledges, so its lag
grows past any bounded window. This bound is unrelated to the observed-state
freshness limit above — one bounds pending convergence across several
reconciles, the other bounds heartbeat staleness — so neither constrains the
other and the grace is expected to be the larger.

JIT registration, Docker start, validated job start, and finalization record
bounded counters and event timestamps. Registration/start/finalization also
record durations; job start records runner-assignment-to-durable-observation
lag. Reconcile spans contain timestamped `worker.registered`, `worker.started`,
and `job.started` events without runner or job identities. `jobs.json` remains
the durable exact-identity ledger for operator diagnostics and retains
`jobStartedAt`, artifact start, completion, and finalization timestamps.
GitHub's runner-assignment timestamp is persisted beside that ledger in
`runner-assign-times.json` (not inside schema-version-1 `jobs.json`) when the
scale-set listener records a job-started or job-completed event, so assignment→create
queue wait stays measurable from local state without breaking rollback readability
for older controllers that reject unknown job-record fields.

## Why the controller is draining

`observed.json` carries `quiesceReason` alongside `drainStartedAt` whenever the
controller holds capacity at zero to drain, and omits the key entirely
otherwise. Diagnosing a stuck `draining` phase previously required reading
`plan.go`'s quiesce path, because observed state recorded only when the drain
started, never why.

The reason names the branch the plan actually took, not the intent behind it:
an operator-disabled drain, a gaming-mode drain while work is active or while
the host is being torn down, excess workers that cannot be represented as
advertised burst capacity, or an exhausted host-wide advertisement budget. A
mechanism name is deliberate — an operator setting `temporaryCapacityOverride`
to zero converges through the same downscale branch as an automatic downscale,
so a value implying "operator versus convergence" would misreport that case.

The reason is also the condition phase selection reads, rather than a field
written beside a separate boolean, so the reported reason cannot disagree with
the decision that produced it. A degraded plan still reports the reason it
computed: a controller that is both wedged and degraded is exactly when the
reason is wanted.

Unlike the pool `updatedAt` reuse described above, this change does add a
top-level key. It rides on the forward-tolerance guarantee described in the
next section: a controller rolled back to a release predating the field
ignores it — the drain clock and the CLI surfaces are unaffected — and simply
reports no reason.

## Observed-state rollback readability

`observed.json` is written by the running controller and read back by whichever
release runs next — after a rollback, that is an older release reading a file
the newer one wrote, and the documented rollback order drains before restoring
the prior pair, so the file on disk at that moment carries whatever the newer
release added. The observed-state reader therefore ignores JSON keys it does
not know, top-level and nested, within `schemaVersion` 1: an additive field in
a newer release degrades to being invisible on the older binary instead of
quarantining the file, forcing a capacity-zero recovery pass, and restarting
the drain clock. `TestObservedToleratesFieldsFromANewerRelease` pins the
guarantee, which holds for rollbacks to any release carrying it.

`schemaVersion` is the deliberate compatibility gate that tolerance does not
weaken. A shape an older release genuinely cannot be trusted to read bumps the
version; an unsupported version is rejected and quarantined, which is the
intended outcome. The trailer check equally still rejects a file carrying more
than one JSON value, and operator-authored `desired.json` keeps strict
decoding so a typo in hand-edited intent fails loudly rather than silently
dropping a field.

## Cancellation and runner shutdown noise

The stock one-job GitHub runner cancels its broker long-poll after a terminal
job. Its diagnostic output can contain `TaskCanceledException` even when the
job succeeded and the container exited zero. The controller does not parse log
text as a job outcome. Worker finalization uses the Docker exit lifecycle, and
job completion uses the validated official scale-set message result. Therefore
that expected broker-shutdown message remains diagnostic text, not a failed job
or a failed worker metric.

Real GitHub cancellations are reported from validated completion events as
`assigned` or `before_assignment`. Controller context cancellation is reported
separately. Worker artifact capture or exporter failures remain runtime errors;
they cannot rewrite a GitHub job result.

## Suggested alerts and dashboard panels

Start with panels for advertised versus assigned versus desired capacity by
pool, active jobs and workers by state, reconcile duration, available memory,
CPU, and both admission gates. Useful alerts include:

- assigned capacity remaining above desired capacity for multiple reconciles;
- advertised capacity at zero while enabled and neither gate is active;
- unacknowledged capacity persisting beyond the acknowledgement grace window
  described above;
- repeated reconcile errors or worker finalization runtime errors;
- sustained resource-gate activation;
- recurring `memory-clamped-capacity` log lines while a `workerMemoryBudget`
  is configured (a correctly sized budget should never clamp); and
- desired workers that remain in `starting` without becoming `idle` or `busy`.

Only explicit controller shutdown is classified as an expected worker
cancellation. Artifact deadlines and persistence cancellations remain
`runtime_error`. Treat a single expected cancellation as lifecycle evidence,
not an alert.

Authoritative configuration references:

- [OpenTelemetry OTLP exporter configuration](https://github.com/open-telemetry/opentelemetry.io/blob/main/content/en/docs/languages/sdk-configuration/otlp-exporter.md)
- [OpenTelemetry Go exporters](https://github.com/open-telemetry/opentelemetry-go/tree/main/exporters)
