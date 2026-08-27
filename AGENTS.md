# AGENTS.md

## Cursor Cloud specific instructions

`ci-runner` is a single Go module (`github.com/melodic-software/ci-runner`, `go 1.27.0`)
that ships two commands: the operator CLI `./cmd/ci-runner` and the windowless
`./cmd/ci-runner-controller`. The product's real runtime target is Windows
(DPAPI, Docker Desktop, WSL, Task Scheduler), but every Windows-only source file
has a `//go:build !windows` (`*_other.go`) counterpart, so the whole module
builds, tests, and lints cleanly on the Linux dev VM. Standard build/test/lint
lanes live in `.github/workflows/ci.yml`; the CLI surface is documented in
`README.md`.

### Go toolchain (non-obvious)

The base `go` on `PATH` is 1.22.2. Go's `GOTOOLCHAIN=auto` reads the `go 1.27.0`
directive in `go.mod` and transparently fetches/execs the go1.27.0 toolchain when
you run `go` from the repo, so `go version` reports 1.27.0 here. Nothing extra is
needed for `go build`/`go test`.

### Lint (critical gotcha)

Lint is golangci-lint v2 driven by `.golangci.yml`. Do **not** use the prebuilt
release binary or `curl | sh` installer: those binaries are built with an older
Go and refuse to run against this go1.27.0 module with
`the Go language version (goX.Y) used to build golangci-lint is lower than the
targeted Go version (1.27.0)`. You must build golangci-lint from source with the
repo's toolchain (it is not part of the startup update script):

```bash
GOTOOLCHAIN=go1.27.0 GOFLAGS=-mod=mod go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.1
golangci-lint run ./...
```

golangci-lint must be **v2.13.1 or newer**: go1.27 support landed in v2.13.0
([golangci-lint#6643](https://github.com/golangci/golangci-lint/issues/6643)),
and older releases reject this module outright. The version is kept aligned with
`GOLANGCI_LINT_VERSION` in the shared `go-quality` reusable that
`.github/workflows/ci.yml` calls, so local lint matches CI.

`GOTOOLCHAIN=go1.27.0` is required — without it, `go install` honors
golangci-lint's own (older) `toolchain` directive and produces a binary that
fails the version check above. Keep the pin aligned with `go.mod`'s `go` line.

### Build / test / run

- Build (Linux, all packages): `go build ./...`
- Cross-compile the shipped Windows executables (same as the `go-windows-build`
  CI lane): `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath ./cmd/...`
- Test with the race detector (matches the reusable go-quality lane):
  `go test -race ./...`
- Run: the CLI resolves a host config **before** dispatching any subcommand
  (even `help`). Pass `--config <abs-path>` or set `CI_RUNNER_CONFIG` /
  `LOCALAPPDATA`. On Linux the runnable core command is
  `ci-runner --config <path> config validate [--json]`; it loads and strictly
  validates the YAML and prints the normalized manifest/paths contract.
  Most `host …` and `secret import` operations require Windows or a live
  controller and are not exercisable on this VM.

The extensive repo-hygiene lanes in `ci.yml` (markdown, shellcheck, shfmt,
typos, editorconfig, actionlint, zizmor, worker-image, etc.) run through
external reusable workflows and are not needed for local Go development.
