package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	dockerruntime "github.com/melodic-software/ci-runner/internal/runtime/docker"
	"github.com/melodic-software/ci-runner/internal/telemetry"
)

type runtimeAccessController interface {
	Harden(string) error
	Verify(string) error
}

func newWorkerRuntime(
	cfg config.Config,
	manifest CompatibilityManifest,
	acl runtimeAccessController,
	jobs jobindex.Store,
	telemetryRecorder telemetry.Recorder,
	onError func(error),
) (*dockerruntime.Runtime, error) {
	artifacts, err := newWorkerArtifactSink(cfg, acl, jobs)
	if err != nil {
		return nil, err
	}
	return dockerruntime.NewLocal(dockerruntime.Options{
		HostID: cfg.Host.ID, ControllerVersion: buildinfo.Version, Image: manifest.WorkerReference(),
		DockerLogMaxSizeBytes: uint64(cfg.Logs.Docker.MaxSize), DockerLogMaxFiles: cfg.Logs.Docker.MaxFiles,
		IdleConfirmationWindow: cfg.Drain.IdleConfirmationWindow.Duration,
		FinalizationTimeout:    cfg.Logs.WorkerFinalizationTimeout.Duration,
		ImagePullTimeout:       cfg.WorkerImage.PullTimeout.Duration,
		Artifacts:              artifacts, OnError: onError, Telemetry: telemetryRecorder,
	})
}

func newWorkerArtifactSink(cfg config.Config, acl runtimeAccessController, jobs jobindex.Store) (*dockerruntime.FileArtifactSink, error) {
	workerLogDirectory := filepath.Join(cfg.Paths.Logs, "workers")
	for _, directory := range []string{workerLogDirectory, cfg.Paths.Diagnostics} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create worker artifact directory %q: %w", directory, err)
		}
		if err := acl.Harden(directory); err != nil {
			return nil, fmt.Errorf("secure worker artifact directory %q: %w", directory, err)
		}
	}
	return dockerruntime.NewFileArtifactSink(workerLogDirectory, cfg.Paths.Diagnostics, jobs, acl, dockerruntime.ArtifactPolicy{
		MaxFileSizeBytes:           uint64(cfg.Logs.Diagnostics.MaxFileSize),
		RawDiagnosticMaxInputBytes: uint64(cfg.Logs.RawDiagnosticMaxInput),
		Retention:                  cfg.Logs.Diagnostics.Retention.Duration,
		TotalCapBytes:              uint64(cfg.Logs.Diagnostics.TotalCap),
		CleanupEvery:               cfg.Logs.CleanupEvery.Duration,
	})
}
