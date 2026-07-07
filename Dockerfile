# catthehacker's act "medium" image: Ubuntu with the toolchain most GitHub-hosted
# actions expect (node, curl, jq, git-lfs, sudo), built for running Actions jobs
# outside GitHub-hosted runners. The pin is the multi-arch INDEX digest (what
# FROM resolves), not a per-platform manifest digest; Dependabot bumps it.
FROM ghcr.io/catthehacker/ubuntu:act-latest@sha256:c710431fbad9eb3bcb102d04e5ff74fbd0ce6e383f78afebfb3770a1a817fdf9

# Bump RUNNER_VERSION + RUNNER_SHA256 together (SHA is in the actions/runner
# release notes). Ephemeral/JIT runners cannot self-update, and GitHub refuses
# registration from versions that fall too far behind — keep this current.
ARG RUNNER_VERSION=2.335.1
ARG RUNNER_SHA256=4ef2f25285f0ae4477f1fe1e346db76d2f3ebf03824e2ddd1973a2819bf6c8cf

# Non-root user mirroring GitHub-hosted runner conventions (uid 1001,
# passwordless sudo) so workflows behave the same here as on ubuntu-latest.
RUN groupadd -g 1001 runner \
 && useradd -u 1001 -g runner -G sudo -m -s /bin/bash runner \
 && echo 'runner ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner

# GitHub-hosted ubuntu preinstalls a C toolchain; the act base does not. .NET
# Native AOT publishing (medley's aot-publish-smoke job) links with clang and
# needs the zlib headers, so bake them in rather than apt-get on every
# ephemeral job.
RUN apt-get update \
 && apt-get install -y --no-install-recommends clang zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*

# setup-dotnet's Ubuntu default is /usr/share/dotnet, which the de-privileged
# runner cannot write (its own README says to point self-hosted runners at a
# user-writable path). The toolcache tree is runner-owned and deliberately
# survives container restarts, so installed SDKs are reused across jobs.
ENV DOTNET_INSTALL_DIR=/opt/hostedtoolcache/dotnet

WORKDIR /home/runner/actions-runner

RUN curl -fsSL -o runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
 && echo "${RUNNER_SHA256}  runner.tar.gz" | sha256sum -c - \
 && tar -xzf runner.tar.gz \
 && rm runner.tar.gz \
 && ./bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && chown -R runner:runner /home/runner

# Root-owned path, NOT under /home/runner: the entrypoint runs as root, and a
# job (running as 'runner') could replace any file in its own writable home to
# hijack the next restart. /usr/local/bin is writable only by root.
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

# No USER directive: the entrypoint must start as root (it scrubs prior-job
# state and holds the App key material where only root can read it), then
# drops to the unprivileged 'runner' user via setpriv before the job runs.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
