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
  public repositories, so fork PRs can never reach these hosts.
- **GitHub App, not PATs.** The only durable credential is the App's private
  key — device-bound, one per host, mounted read-only, revocable per host.
  Everything else (JWT, installation token, JIT config) is minted per
  container start and dies with it.
- **Least privilege.** The App needs only the organization
  **Self-hosted runners: write** permission, and the installation token is
  downscoped to exactly that at mint time.

## Host setup

1. Create (once, org-wide) a dedicated **runner-registration GitHub App**:
   org permission *Self-hosted runners: write*, no repo permissions, no
   webhook. Install it on the org.
2. On each runner host: generate a **new private key** in the App settings,
   store it under the user profile (e.g.
   `%USERPROFILE%\.ci-runner\github-app-key.pem` — not `ProgramData`, whose
   subdirectories all local users can read), and never sync or commit it.
3. Copy `.env.example` to `.env` beside `docker-compose.yml`, fill in the
   client ID, key path, and replica count.
4. `docker compose up -d`. Boot-time start and image refresh are wired by the
   host's provisioning (see the `provisioning` repo).

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `APP_CLIENT_ID` | *(required)* | GitHub App client ID (JWT issuer) |
| `APP_PRIVATE_KEY_PATH` | *(required)* | Host path of this device's App private key |
| `RUNNER_REPLICAS` | `2` | Concurrent one-job runner containers |
| `RUNNER_LABELS` | `self-hosted-medley` | Comma-separated `runs-on` routing labels |
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
