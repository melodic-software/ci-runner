# Deferred fleet capabilities

This file records deliberate V1 boundaries. An item here is not an accepted
workaround and does not weaken the current worker contract. Each capability
requires a separate design review, threat model, compatibility proof, and
canary before a workflow may depend on it.

## Standing non-goals

The two-host fleet does not need Kubernetes or an Actions Runner Controller
deployment. It also does not accept privileged workers, host Docker-socket
passthrough, a persistent native Windows runner on a daily-use host, Docker
Desktop engine switching, mutable `latest` deployment, or an external watchdog
that cancels and replays workflow side effects. Reconsidering one of these is an
architecture change, not routine configuration.

## Isolated Linux VM backend

Testcontainers, service containers, job containers, Docker actions, and other
Docker-daemon workloads remain GitHub-hosted. A future backend may run them in
a disposable Linux VM whose Docker daemon, filesystem, and runner identity are
destroyed after one job. The host Docker socket must never be mounted or proxied
into a worker. The backend must preserve JIT registration, external diagnostics,
resource admission, graceful drain, and the audited hosted-only cutoff with a
full workflow rerun that recomputes selector eligibility.

This work is motivated by GitHub's warning that self-hosted runners can be
persistently compromised by workflow code and Docker's warning that daemon
access is equivalent to highly privileged host access:

- [GitHub secure use of self-hosted runners](https://docs.github.com/en/actions/reference/security/secure-use#hardening-for-self-hosted-runners)
- [Docker daemon attack surface](https://docs.docker.com/engine/security/#docker-daemon-attack-surface)

## Ephemeral Windows VM backend

Windows-only tests remain on `windows-2025`. A future Hyper-V adapter may create
a Windows VM and disposable disks from a verified patched base, inject one JIT
runner configuration, execute one job, export diagnostics, and destroy the VM
and its disposable disks. Image licensing, Windows Update, base-image integrity,
host/guest boundaries, and unattended failure recovery must be proven before
adoption. Any alternative reset mechanism remains out of scope until Microsoft
documents it and a separate threat model proves equivalent isolation.

V1 will not register a persistent native Windows runner on a daily-use host and
will not switch Docker Desktop between Linux and Windows container engines.

- [Microsoft Hyper-V security planning](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/plan/plan-hyper-v-security-in-windows-server)
- [GitHub self-hosted runner reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)

## Stronger worker network isolation

V1 workers require outbound network access for GitHub Actions, source checkout,
tool setup, packages, caches, and artifacts. A future network adapter may add
audited DNS/egress policy or a proxy, but it must measure action compatibility,
fail visibly, keep controller credentials unreachable, and avoid claiming an
isolation guarantee the Docker network does not enforce.

No future network layer may justify privileged workers, host devices, host
paths, or Docker-socket access.

- [GitHub runner communication requirements](https://docs.github.com/en/actions/reference/runners/self-hosted-runners#requirements-for-communication-with-github)
- [GitHub proxy configuration for runners](https://docs.github.com/en/actions/how-tos/manage-runners/use-proxy-servers)
- [Docker packet filtering and firewalls](https://docs.docker.com/engine/network/packet-filtering-firewalls/)

## Optional OTLP export

The controller now provides optional OTLP trace and metric export, disabled by
default, alongside JSON Lines and archived runner diagnostics. Configuration,
redaction, failure isolation, lifecycle behavior, and collector guidance are
documented in [Observability](observability.md).

- [OpenTelemetry protocol specification](https://opentelemetry.io/docs/specs/otlp/)

## Independent monitor and cost evidence

The public scheduled queue monitor cannot prove its own freshness. A future
independent control plane may alert when that schedule is disabled or stale,
but it must not depend on the monitored schedule or the local fleet.

Cost reporting may automate the same GitHub billing-usage summary used for the
rollout baseline. It must keep private billing data out of this public
repository, separate selector spend from workload spend, and normalize by
eligible completed jobs before claiming savings.

- [GitHub billing usage API](https://docs.github.com/en/rest/billing/usage)
- [GitHub scheduled-event limitations](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#schedule)
