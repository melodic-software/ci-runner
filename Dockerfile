# catthehacker's act "medium" image: Ubuntu with the toolchain most GitHub-hosted
# actions expect (node, curl, jq, git-lfs, sudo), built for running Actions jobs
# outside GitHub-hosted runners.
FROM ghcr.io/catthehacker/ubuntu:act-latest

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

WORKDIR /home/runner/actions-runner

RUN curl -fsSL -o runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
 && echo "${RUNNER_SHA256}  runner.tar.gz" | sha256sum -c - \
 && tar -xzf runner.tar.gz \
 && rm runner.tar.gz \
 && ./bin/installdependencies.sh \
 && rm -rf /var/lib/apt/lists/* \
 && chown -R runner:runner /home/runner

COPY --chown=runner:runner --chmod=0755 entrypoint.sh /home/runner/entrypoint.sh

USER runner
ENTRYPOINT ["/home/runner/entrypoint.sh"]
