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
completed reconciliation; counters are monotonic process-lifetime events.

| Metric | Meaning | Attributes |
| --- | --- | --- |
| `ci_runner.controller.reconcile.duration` | Reconcile duration in seconds | `ci_runner.reconcile.result` |
| `ci_runner.controller.reconcile.errors` | Unexpected reconcile failures | none |
| `ci_runner.controller.observed.checkpoint.age` | Prior durable checkpoint age at reconcile start; omitted when missing, corrupt, or future-dated | none |
| `ci_runner.capacity.advertised` | Capacity acknowledged to GitHub | `ci_runner.pool.id` |
| `ci_runner.capacity.acknowledged` | Latest target capacity is listener-acknowledged, `0` or `1` | `ci_runner.pool.id` |
| `ci_runner.capacity.acknowledgement.pending.age` | Age of a pending capacity transition; omitted when acknowledged or unavailable | `ci_runner.pool.id` |
| `ci_runner.capacity.assigned` | Authoritative assigned jobs | `ci_runner.pool.id` |
| `ci_runner.capacity.desired` | Desired local workers | `ci_runner.pool.id` |
| `ci_runner.workers` | Workers in each bounded state | `ci_runner.pool.id`, `ci_runner.worker.state` |
| `ci_runner.jobs.active` | Busy workers/active jobs | `ci_runner.pool.id` |
| `ci_runner.docker.inventory.workers` | Reconciled Docker inventory count | `ci_runner.pool.id` |
| `ci_runner.accounting.assignment.gap` | Assigned minus visible busy/starting workers | `ci_runner.pool.id` |
| `ci_runner.accounting.transient_lag` | Bounded short-job lag classification, `0` or `1` | `ci_runner.pool.id` |
| `ci_runner.host.cpu.utilization` | Host CPU utilization percent | none |
| `ci_runner.host.memory.available` | Available physical memory in bytes | none |
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
as healthy for one configured listener request timeout plus two reconciliation
intervals; a transition still pending after that bounded grace is unhealthy.
This reuses the existing schema-version-1 pool `updatedAt` field and does not
change the rollback-readable observed-state shape.

JIT registration, Docker start, validated job start, and finalization record
bounded counters and event timestamps. Registration/start/finalization also
record durations; job start records runner-assignment-to-durable-observation
lag. Reconcile spans contain timestamped `worker.registered`, `worker.started`,
and `job.started` events without runner or job identities. `jobs.json` remains
the durable exact-identity ledger for operator diagnostics and retains
`jobStartedAt`, artifact start, completion, and finalization timestamps.

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
- unacknowledged capacity persisting beyond one listener request timeout plus
  two reconciliation intervals;
- repeated reconcile errors or worker finalization runtime errors;
- sustained resource-gate activation; and
- desired workers that remain in `starting` without becoming `idle` or `busy`.

Only explicit controller shutdown is classified as an expected worker
cancellation. Artifact deadlines and persistence cancellations remain
`runtime_error`. Treat a single expected cancellation as lifecycle evidence,
not an alert.

Authoritative configuration references:

- [OpenTelemetry OTLP exporter configuration](https://opentelemetry.io/docs/languages/sdk-configuration/otlp-exporter/)
- [OpenTelemetry Go exporters](https://opentelemetry.io/docs/languages/go/exporters/)
