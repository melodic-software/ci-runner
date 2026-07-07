# ci-runner

Self-hosted GitHub Actions runner image — primary CI compute for the org's
**private** repos, with GitHub-hosted fallback. Published to
`ghcr.io/melodic-software/ci-runner`.

## How it works

Each container is an **ephemeral just-in-time runner**: on start,
`entrypoint.sh` authenticates as a GitHub App, mints an installation token
downscoped to `organization_self_hosted_runners: write`, resolves the
`self-hosted` runner group, requests a JIT runner config, and `exec`s the
runner. A JIT runner accepts **exactly one job** and exits; Docker's
`restart: always` then starts a fresh container, which registers anew. Every
job therefore gets a clean environment, and no long-lived registration token
or runner state survives between jobs.

The image is `ghcr.io/catthehacker/ubuntu:act-latest` (the toolchain most
actions expect) plus a pinned [`actions/runner`](https://github.com/actions/runner)
and a non-root `runner` user (uid 1001, passwordless sudo) mirroring
GitHub-hosted conventions.

## Security model

- **Private repos only.** The `self-hosted` org runner group (governed by
  [`github-iac`](https://github.com/melodic-software/github-iac)) blocks
  public repositories, so fork PRs can never reach these hosts. Everything
  that runs here is this org's own first-party code — that trust assumption
  underpins the rest of this section.
- **GitHub App, not PATs.** The only durable credential is the App's private
  key — device-bound, one per host, revocable per host. Everything else (JWT,
  installation token, JIT config) is minted per container start and dies with
  it.
- **Least privilege.** The App needs only the organization
  **Self-hosted runners: write** permission, and the installation token is
  downscoped to exactly that at mint time. A stolen key can register rogue
  runners — nothing else — and revoking that host's key ends it.
- **Key handling.** The key reaches the container base64-encoded in the
  environment, is consumed via process substitution for one JWT signature
  (never written to any filesystem), and is dropped from the live environment
  before the job starts. The entrypoint holds it as root; the runner — and
  every job step — runs de-privileged as `runner`.
- **Accepted residual.** Workflows keep passwordless `sudo` (GitHub-hosted
  parity; medley's dotnet lane trusts its dev cert with it), and root can read
  PID 1's original environment — so a *malicious* job could recover the key.
  Accepted deliberately: the group admits only this org's private repos, the
  key's blast radius is runner registration, and per-host revocation is one
  click. Revisit before this fleet ever serves less-trusted code.
- **Job hygiene.** The work dir and temp are scrubbed before each
  registration. `/opt/hostedtoolcache` **and the runner's `$HOME`** (dotfiles,
  `~/.nuget`, other per-user caches) deliberately survive container restarts —
  re-downloading toolchains and packages every job costs more than the
  poisoning risk under the same trusted-code assumption; image updates
  recreate containers outright. If the fleet ever serves less-trusted code,
  the escape hatch is a recreate-per-job supervisor, not a longer scrub list.

## Host setup

1. Create (once, org-wide) a dedicated **runner-registration GitHub App**:
   org permission *Self-hosted runners: write*, no repo permissions, no
   webhook. Install it on the org.
2. On each runner host: generate a **new private key** in the App settings,
   store it under the user profile (e.g.
   `%USERPROFILE%\.ci-runner\github-app-key.pem` — not `ProgramData`, whose
   subdirectories all local users can read), and never sync or commit it.
3. Copy `.env.example` to `.env` beside `docker-compose.yml`, fill in the
   client ID, the base64-encoded key (one-liner in `.env.example`), and the
   replica count.
4. `docker compose up -d`. Boot-time start and image refresh are wired by the
   host's provisioning (see the `provisioning` repo).

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `APP_CLIENT_ID` | *(required)* | GitHub App client ID (JWT issuer) |
| `APP_PRIVATE_KEY_B64` | *(required)* | This device's App private key, base64-encoded |
| `RUNNER_REPLICAS` | `2` | Concurrent one-job runner containers |
| `RUNNER_LABELS` | `self-hosted-medley` | Comma-separated `runs-on` routing labels. JIT runners get **no implicit labels** (`self-hosted`/`Linux`/`X64`), deliberately: workflows target this fleet only by naming its explicit label |
| `ORG` | `melodic-software` | Organization to register with |
| `RUNNER_GROUP` | `self-hosted` | Runner group to join |

## Publishing

`publish.yml` builds on every PR (validation only) and pushes `latest` +
commit-SHA tags on merge to `main`, plus a weekly scheduled rebuild so the
floating base image stays patched.

`actions/runner` is pinned by version + checksum in the `Dockerfile`.
Ephemeral runners cannot self-update and GitHub eventually refuses versions
that fall too far behind, so bump `RUNNER_VERSION`/`RUNNER_SHA256` (release
notes carry the SHA) when a new runner ships.

## Limitations

- Jobs run *inside* the container: no Docker CLI, no nested containers. A
  workflow that needs Docker (e.g. Testcontainers) must stay on GitHub-hosted
  runners until docker-in-docker or socket passthrough is deliberately added.
- Linux x64 only.
