#!/usr/bin/env bash
set -Eeuo pipefail

/usr/local/libexec/ci-runner-capture-cgroup
/usr/local/libexec/ci-runner-set-state completed
