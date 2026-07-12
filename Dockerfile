# The tag and multi-platform index digest are intentionally both pinned. The
# official image is Ubuntu 24.04 and contains the exact runner binary named by
# the tag. release/dependencies.json records the independent release evidence.
FROM ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29

ARG POWERSHELL_VERSION=7.6.3
ARG POWERSHELL_SHA256=856d0765d2332377f9d7a4aea76efdfde4de51446e7738dde2dfda41dba9e2a7

USER root

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Keep the compatibility layer deliberately small. Versioned language runtimes
# belong in their official setup actions so hosted and self-hosted workflows use
# the same declarations. PowerShell is installed from Microsoft's checksummed
# release asset because it is not in Ubuntu's first-party repository.
RUN apt-get update \
 && apt-get install --yes --no-install-recommends \
      ca-certificates \
      clang \
      curl \
      git \
      git-lfs \
      jq \
      openssh-client \
      sudo \
      unzip \
      zip \
      zlib1g-dev \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      --output /tmp/powershell.tar.gz \
      "https://github.com/PowerShell/PowerShell/releases/download/v${POWERSHELL_VERSION}/powershell-${POWERSHELL_VERSION}-linux-x64.tar.gz" \
 && echo "${POWERSHELL_SHA256}  /tmp/powershell.tar.gz" | sha256sum --check --strict \
 && install --directory --owner=root --group=root --mode=0755 /opt/microsoft/powershell/7 \
 && tar --extract --gzip --file=/tmp/powershell.tar.gz --directory=/opt/microsoft/powershell/7 \
 && chmod 0755 /opt/microsoft/powershell/7/pwsh \
 && ln --symbolic /opt/microsoft/powershell/7/pwsh /usr/local/bin/pwsh \
 && git lfs install --system \
 && rm --force /tmp/powershell.tar.gz \
 && rm --recursive --force /var/lib/apt/lists/*

COPY --chmod=0555 worker/set-state.sh /usr/local/libexec/ci-runner-set-state
COPY --chmod=0555 worker/job-started.sh /usr/local/libexec/ci-runner-job-started.sh
COPY --chmod=0555 worker/job-completed.sh /usr/local/libexec/ci-runner-job-completed.sh
COPY --chmod=0555 worker/entrypoint.sh /usr/local/bin/ci-runner-entrypoint

ENV ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
    ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/libexec/ci-runner-job-started.sh \
    ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/usr/local/libexec/ci-runner-job-completed.sh \
    ImageOS=ubuntu24

LABEL org.opencontainers.image.source="https://github.com/melodic-software/ci-runner" \
      org.opencontainers.image.base.name="ghcr.io/actions/actions-runner:2.335.1" \
      org.opencontainers.image.base.digest="sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29" \
      org.opencontainers.image.description="Ephemeral one-job GitHub Actions worker for ci-runner"

# The upstream user is uid/gid 1001 and has passwordless sudo, matching the
# official Actions runner container. The controller must not override this,
# mount host paths, attach devices, expose the Docker socket, or use privileged
# mode. It supplies the one-job JIT configuration only through the documented
# ACTIONS_RUNNER_INPUT_JITCONFIG environment variable.
USER runner
WORKDIR /home/runner
ENTRYPOINT ["/usr/local/bin/ci-runner-entrypoint"]
CMD ["/home/runner/run.sh"]
