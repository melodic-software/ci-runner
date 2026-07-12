# Worker image contract

The worker is a disposable execution environment, not a credentialed control
plane. The Windows controller creates it only after GitHub returns a one-job JIT
configuration, streams that value over pre-start attached stdin, and starts
`/home/runner/run.sh`. The entrypoint exposes it to the official runner through
the documented `ACTIONS_RUNNER_INPUT_JITCONFIG` input, while keeping it out of
Docker's persistent container configuration and `docker inspect`. The container
is deleted after that job reaches a terminal state. No bootstrap or registration
script runs inside the worker.

## Reviewed upstream baseline

- Final base: `ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29`.
- The tag is GitHub's runner release; the digest is the GHCR multi-platform index.
- The selected image is Ubuntu 24.04 and runs as the upstream `runner` identity
  (uid/gid 1001).
- The scale-set resource is created with `RunnerSetting.DisableUpdate=true`.
  GitHub requires an update-disabled runner to be refreshed within 30 days and
  may block it sooner for a critical release.
- PowerShell comes from Microsoft's checksummed `v7.6.3` Linux x64 release
  archive. That is the version in GitHub's Ubuntu 24.04 hosted-image manifest at
  reviewed commit `f45762e93bedc498afdc31c756e725cc04ee4dee`. The remaining
  compatibility packages come from Ubuntu 24.04.

The immutable evidence and original source URLs are recorded in
`release/dependencies.json`. The daily drift workflow independently resolves the
official feeds and opens or refreshes an issue when a reviewed pin is behind.

Authoritative references:

- [GitHub self-hosted runner reference](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)
- [Official runner image package](https://github.com/actions/actions-runner/pkgs/container/actions-runner)
- [Runner Scale Set Client Docker example](https://github.com/actions/scaleset/tree/v0.4.0/examples/dockerscaleset)
- [GitHub pre-job and post-job hooks](https://docs.github.com/en/actions/how-tos/manage-runners/self-hosted-runners/run-scripts)
- [GitHub runner releases](https://github.com/actions/runner/releases)
- [`setup-dotnet` v5.4.0 rootless installation guidance](https://github.com/actions/setup-dotnet/blob/26b0ec14cb23fa6904739307f278c14f94c95bf1/README.md#environment-variables)
- [`setup-dotnet` v5.4.0 install-directory implementation](https://github.com/actions/setup-dotnet/blob/26b0ec14cb23fa6904739307f278c14f94c95bf1/src/installer.ts#L329-L359)
- [.NET install-script environment contract](https://learn.microsoft.com/en-us/dotnet/core/tools/dotnet-install-script#set-environment-variables)
- [GitHub-hosted Ubuntu 24.04 software manifest](https://github.com/actions/runner-images/blob/f45762e93bedc498afdc31c756e725cc04ee4dee/images/ubuntu/Ubuntu2404-Readme.md)
- [PowerShell releases](https://github.com/PowerShell/PowerShell/releases)
- [Docker image pinning guidance](https://docs.docker.com/build/building/best-practices/#pin-base-image-versions)

## Installed compatibility surface

The derived layer contains only:

- CA certificates, curl, jq, zip/unzip, and OpenSSH client;
- Git and Git LFS;
- sudo, retaining the upstream passwordless-sudo behavior used by Actions
  runner containers;
- PowerShell;
- clang and zlib headers for native compilation.

.NET, Node.js, Python, and other versioned toolchains are deliberately absent.
Workflows install them with their official setup actions exactly as hosted jobs
do. The image provides writable installation locations, not a second, silently
drifting toolchain or toolcache.

## Rootless .NET setup contract

The current official `actions/setup-dotnet` Linux default is
`/usr/share/dotnet`. Its own documentation warns that this default can be
unwritable on self-hosted Linux runners and directs operators to set
`DOTNET_INSTALL_DIR` to a user-writable path. The action passes that environment
variable to its bundled install script, adds the resolved directory to the job
`PATH`, and exports the same directory as `DOTNET_ROOT` after installation.

The image therefore establishes these paths before any workflow step runs:

- `DOTNET_INSTALL_DIR=/home/runner/.dotnet` makes every native-architecture
  `setup-dotnet` SDK/runtime installation rootless;
- `DOTNET_ROOT=/home/runner/.dotnet` gives apphosts and manually installed .NET
  the same runtime root even outside the setup action;
- `/home/runner/.dotnet` and `/home/runner/.dotnet/tools` lead `PATH`, covering
  both the SDK host and `dotnet tool install --global` output; and
- `NUGET_PACKAGES=/home/runner/.nuget/packages` makes restore/cache behavior
  explicit instead of relying on home-directory inference.

Those directories are created in the immutable layer as `runner:runner` with
mode `0755`. They live only in the disposable worker writable layer. A workflow
may still override these variables for a deliberate per-job layout, but the
default can never redirect an unprivileged install or restore to
`/usr/share/dotnet`.

The image intentionally does not set `RUNNER_TOOL_CACHE`,
`RUNNER_TOOLSDIRECTORY`, or `AGENT_TOOLSDIRECTORY`. The pinned official runner
[resolves its tool cache from the configured work directory and creates it when
the job starts](https://github.com/actions/runner/blob/7d737449ef346f6524f75688d0c9c95fa10ba10a/src/Runner.Worker/JobRunner.cs#L174-L176),
while [`setup-dotnet` installs through `DOTNET_INSTALL_DIR`](https://github.com/actions/setup-dotnet/blob/26b0ec14cb23fa6904739307f278c14f94c95bf1/src/installer.ts#L329-L359).
Keeping those runner variables dynamic avoids coupling the image to a work
folder chosen by the one-job JIT configuration; the non-root runner process owns
the disposable work tree it creates.

## Mandatory runtime invariants

The controller must create every worker with all of these properties:

- fresh container and writable layer for exactly one job;
- user `runner`, not root;
- no bind mounts or named volumes, including `_work`, home, temp, toolcache, or
  diagnostic paths;
- no Docker socket, host path, device, GPU, or privileged mode;
- no GitHub App key, observer key, controller JWT, installation token, or other
  host credential in environment variables, arguments, labels, or files;
- no encoded JIT configuration in Docker config, inspection output, process
  arguments, controller state, or controller logs;
- CPU, memory, memory-plus-swap, and PID limits from validated host config;
- Docker `local` logging with `max-size=10m` and `max-file=3`;
- runner stdout/stderr captured by the controller and `_diag` copied out before
  deletion through Docker's archive API, never through a host mount.

Container deletion is evidence-gated: the controller must durably index the
job/runner/container identity, drain stdout even after its configured output
cap, bound both raw `_diag` input and compressed output, publish both artifacts,
and finalize the catalog record first. Any persistence failure retains the
exited container for adoption and retry after controller restart. Retention
skips every open or freshly adopted record, removes the oldest finalized
records first, and writes a durable tombstone so deleted files are not
rediscovered as live evidence.

Jobs using service containers, job containers, Testcontainers, or a Docker
socket are outside this image's contract and stay GitHub-hosted.

The stock runner necessarily decodes the one-job JIT payload into its disposable
runner configuration files before connecting, and runner processes initially
receive that value as their documented input. Workflow code runs under the same
container identity and must therefore be treated as able to inspect its own
ephemeral, one-job runner credentials. This is not a host trust boundary. The
security boundary is that no reusable App key, observer credential, controller
JWT, installation token, Docker control socket, or host filesystem enters the
worker; the writable layer and its one-job credentials are destroyed after use.

For controller restart recovery, the image uses GitHub's documented
`ACTIONS_RUNNER_HOOK_JOB_STARTED` and `ACTIONS_RUNNER_HOOK_JOB_COMPLETED`
hooks. A root-owned entrypoint atomically initializes
`/home/runner/_runner_state/state` to `idle` immediately before starting the
runner; the hooks atomically replace it with `busy` and `completed`. The file is
inside the disposable writable layer and is copied through Docker's archive API;
it is never mounted from the host. After recording `completed`, the terminal
hook snapshots cgroup-v2 `memory.peak`, `memory.swap.peak`, OOM events,
`cpu.stat` periods and throttling, `pids.peak`, and aggregate `io.stat` bytes
into a bounded, schema-versioned JSON file in the same state directory. It does
not sample or resize the worker, and it never removes or interrupts the
container, consistent with GitHub's warning that a post-job hook is not an
autoscaler teardown mechanism.

The controller copies and strictly validates that terminal resource evidence
before container removal and persists it beside the copied diagnostics. A
missing Docker archive endpoint, absent cgroup field, or invalid payload creates
a bounded controller-owned fallback record; it does not infer a zero peak or a
job failure. Failure to durably persist either the fallback or real evidence
retains the exited container for the same evidence retry contract as logs and
`_diag`. Workflow code shares the disposable runner identity and can alter its
own writable layer, so cgroup evidence is capacity-tuning telemetry, not a
security attestation.

`scripts/verify-worker-image.sh` verifies the immutable image metadata, approved
tool surface, exact runner version, non-root identity, rootless .NET/NuGet
environment and directory ownership, absence of runner toolcache overrides,
absence of baked JIT or credential variables, required stdin handoff,
passwordless sudo, and the exact `idle → busy → completed` hook sequence.
Runtime isolation is verified in the controller adapter tests and live canary
because it is a container-create contract rather than an image property.

`completed` means workflow steps have finished; GitHub runs the completed hook
before its final job protocol finishes. The controller must still wait for
container exit or the authoritative Scale Set Client completion signal. It may
never use `completed` alone as permission to terminate a running container.
