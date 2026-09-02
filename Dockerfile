# The tag and multi-platform index digest are intentionally both pinned. The
# official image is Ubuntu 24.04 and contains the exact runner binary named by
# the tag. release/dependencies.json records the independent release evidence.
FROM ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4

ARG POWERSHELL_VERSION=7.6.5
ARG POWERSHELL_SHA256=b34ab3b19acac1d3d4d0d3cfdb02acf62f457b0b6a962ff008132033f7566844
ARG GH_VERSION=2.97.0
ARG GH_SHA256=a2c9b8497e1f85b1ad0dfcb78b5a622e098801b8e461e459e88e1ee12f018112

USER root

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Keep the compatibility layer deliberately small. Versioned language runtimes
# belong in their official setup actions so hosted and self-hosted workflows use
# the same declarations. PowerShell is installed from Microsoft's checksummed
# release asset because it is not in Ubuntu's first-party repository. The
# GitHub CLI is installed from its checksummed official release asset at the
# hosted-image manifest version; Ubuntu's archive carries a years-stale gh.
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
      zstd \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      --output /tmp/powershell.tar.gz \
      "https://github.com/PowerShell/PowerShell/releases/download/v${POWERSHELL_VERSION}/powershell-${POWERSHELL_VERSION}-linux-x64.tar.gz" \
 && echo "${POWERSHELL_SHA256}  /tmp/powershell.tar.gz" | sha256sum --check --strict \
 && install --directory --owner=root --group=root --mode=0755 /opt/microsoft/powershell/7 \
 && tar --extract --gzip --file=/tmp/powershell.tar.gz --directory=/opt/microsoft/powershell/7 \
 && chmod 0755 /opt/microsoft/powershell/7/pwsh \
 && ln --symbolic /opt/microsoft/powershell/7/pwsh /usr/local/bin/pwsh \
 && curl --fail --location --proto '=https' --tlsv1.2 \
      --output /tmp/gh.tar.gz \
      "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz" \
 && echo "${GH_SHA256}  /tmp/gh.tar.gz" | sha256sum --check --strict \
 && tar --extract --gzip --file=/tmp/gh.tar.gz --directory=/usr/local/bin \
      --strip-components=2 "gh_${GH_VERSION}_linux_amd64/bin/gh" \
 && chmod 0755 /usr/local/bin/gh \
 && git lfs install --system \
 && install --directory --owner=runner --group=runner --mode=0755 \
      /home/runner/.dotnet \
      /home/runner/.dotnet/tools \
      /home/runner/.nuget \
      /home/runner/.nuget/packages \
 && rm --force /tmp/powershell.tar.gz /tmp/gh.tar.gz \
 && rm --recursive --force /var/lib/apt/lists/*

COPY --chmod=0555 worker/set-state.sh /usr/local/libexec/ci-runner-set-state
COPY --chmod=0555 worker/job-started.sh /usr/local/libexec/ci-runner-job-started.sh
COPY --chmod=0555 worker/job-completed.sh /usr/local/libexec/ci-runner-job-completed.sh
COPY --chmod=0555 worker/capture-cgroup.sh /usr/local/libexec/ci-runner-capture-cgroup
COPY --chmod=0555 worker/entrypoint.sh /usr/local/bin/ci-runner-entrypoint

# actions/setup-dotnet otherwise selects /usr/share/dotnet on Linux. Keep SDK,
# global-tool, and NuGet writes inside the disposable non-root runner home.
ENV DOTNET_INSTALL_DIR=/home/runner/.dotnet \
    DOTNET_ROOT=/home/runner/.dotnet \
    NUGET_PACKAGES=/home/runner/.nuget/packages \
    PATH=/home/runner/.dotnet:/home/runner/.dotnet/tools:${PATH} \
    ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1 \
    ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/libexec/ci-runner-job-started.sh \
    ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/usr/local/libexec/ci-runner-job-completed.sh \
    ImageOS=ubuntu24

LABEL org.opencontainers.image.source="https://github.com/melodic-software/ci-runner" \
      org.opencontainers.image.base.name="ghcr.io/actions/actions-runner:2.336.0" \
      org.opencontainers.image.base.digest="sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda" \
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
