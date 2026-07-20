// Package docker is the sole worker-container lifecycle implementation. It
// uses the official Moby Docker Engine client and exposes controller ports only.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/telemetry"
	"github.com/moby/moby/api/pkg/stdcopy"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	managedLabel                = "com.melodic-software.ci-runner.managed"
	hostLabel                   = "com.melodic-software.ci-runner.host"
	poolLabel                   = "com.melodic-software.ci-runner.pool"
	workerNameLabel             = "com.melodic-software.ci-runner.name"
	resourceTierLabel           = "com.melodic-software.ci-runner.resource-tier"
	runnerIDLabel               = "com.melodic-software.ci-runner.github-runner-id"
	startedAtLabel              = "com.melodic-software.ci-runner.started-at"
	controllerLabel             = "com.melodic-software.ci-runner.controller-version"
	composeProjectLabel         = "com.docker.compose.project"
	composeServiceLabel         = "com.docker.compose.service"
	composeNumberLabel          = "com.docker.compose.container-number"
	composeOneoffLabel          = "com.docker.compose.oneoff"
	composeServiceName          = "worker"
	defaultStatePath            = "/home/runner/_runner_state/state"
	defaultDiagPath             = "/home/runner/_diag"
	defaultResourceEvidencePath = "/home/runner/_runner_state/cgroup-terminal.json"
	maximumStateBytes           = 32
)

var digestPinnedImage = regexp.MustCompile(`^[^\s@]+(?::[^\s@]+)?@sha256:[0-9a-f]{64}$`)

// Engine is a narrow seam over the official Moby client for deterministic
// tests. *client.Client structurally implements it.
type Engine interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Options struct {
	HostID                 string
	ControllerVersion      string
	Image                  string
	StatePath              string
	DiagnosticPath         string
	ResourceEvidencePath   string
	DockerLogMaxSizeBytes  uint64
	DockerLogMaxFiles      int
	IdleConfirmationWindow time.Duration
	FinalizationTimeout    time.Duration
	ImagePullTimeout       time.Duration
	Artifacts              ArtifactSink
	OnError                func(error)
	Telemetry              telemetry.Recorder
}

func (o *Options) defaults() {
	if o.StatePath == "" {
		o.StatePath = defaultStatePath
	}
	if o.DiagnosticPath == "" {
		o.DiagnosticPath = defaultDiagPath
	}
	if o.ResourceEvidencePath == "" {
		o.ResourceEvidencePath = defaultResourceEvidencePath
	}
	if o.OnError == nil {
		o.OnError = func(error) {}
	}
	if o.Telemetry == nil {
		o.Telemetry = telemetry.Noop()
	}
}

func (o Options) validate() error {
	if o.HostID == "" || o.ControllerVersion == "" {
		return errors.New("docker runtime host ID and controller version are required")
	}
	if !digestPinnedImage.MatchString(o.Image) {
		return errors.New("docker worker image must be pinned to an exact sha256 manifest digest")
	}
	if !strings.HasPrefix(o.StatePath, "/") || !strings.HasPrefix(o.DiagnosticPath, "/") || !strings.HasPrefix(o.ResourceEvidencePath, "/") {
		return errors.New("worker state, diagnostic, and resource-evidence paths must be absolute Linux paths")
	}
	if o.DockerLogMaxSizeBytes == 0 || o.DockerLogMaxFiles <= 0 {
		return errors.New("docker local log size and file count must be positive")
	}
	if o.IdleConfirmationWindow <= 0 || o.FinalizationTimeout <= 0 {
		return errors.New("idle confirmation and finalization timeouts must be positive")
	}
	if o.ImagePullTimeout <= 0 {
		return errors.New("image pull timeout must be positive")
	}
	if o.Artifacts == nil {
		return errors.New("worker artifact sink is required")
	}
	return nil
}

type Runtime struct {
	engine Engine
	opts   Options

	ctx    context.Context
	cancel context.CancelFunc
	close  func() error

	mu      sync.Mutex
	watches map[string]*containerWatch
	wg      sync.WaitGroup
}

type containerWatch struct {
	done chan struct{}
	err  error
}

func New(engine Engine, options Options) (*Runtime, error) {
	if engine == nil {
		return nil, errors.New("docker engine client is required")
	}
	options.defaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{engine: engine, opts: options, ctx: ctx, cancel: cancel, close: func() error { return nil }, watches: map[string]*containerWatch{}}, nil
}

// NewLocal creates a client for the platform's fixed local Docker Engine
// endpoint. Environment overrides are intentionally ignored: forwarding a JIT
// configuration to an arbitrary DOCKER_HOST would cross the host boundary.
func NewLocal(options Options) (*Runtime, error) {
	apiClient, err := newLocalClient(options.ControllerVersion)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	runtime, err := New(apiClient, options)
	if err != nil {
		_ = apiClient.Close()
		return nil, err
	}
	runtime.close = apiClient.Close
	return runtime, nil
}

// ProbeLocal inspects only Docker Desktop's fixed local Engine endpoint. It
// intentionally ignores DOCKER_HOST and related environment overrides so a
// health check cannot silently validate a remote daemon.
func ProbeLocal(ctx context.Context, controllerVersion string) (string, string, error) {
	apiClient, err := newLocalClient(controllerVersion)
	if err != nil {
		return "", "", fmt.Errorf("create Docker client: %w", err)
	}
	result, infoErr := apiClient.Info(ctx, client.InfoOptions{})
	closeErr := apiClient.Close()
	if infoErr != nil {
		return "", "", errors.Join(fmt.Errorf("inspect local Docker Engine: %w", infoErr), closeErr)
	}
	if closeErr != nil {
		return result.Info.OSType, result.Info.Architecture, fmt.Errorf("close local Docker Engine client: %w", closeErr)
	}
	if err := validateEngineInfo(result.Info.OSType, result.Info.Architecture); err != nil {
		return result.Info.OSType, result.Info.Architecture, err
	}
	return result.Info.OSType, result.Info.Architecture, nil
}

// InventoryLocalArtifacts returns only managed containers belonging to this
// host from Docker Desktop's fixed local endpoint. It is used by the explicit
// retention command so live and exited retry sources are never deleted.
func InventoryLocalArtifacts(ctx context.Context, controllerVersion, hostID string) ([]ArtifactMetadata, error) {
	if strings.TrimSpace(hostID) == "" {
		return nil, errors.New("host ID is required for local Docker inventory")
	}
	apiClient, err := newLocalClient(controllerVersion)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	info, infoErr := apiClient.Info(ctx, client.InfoOptions{})
	if infoErr != nil {
		return nil, errors.Join(fmt.Errorf("inspect local Docker Engine: %w", infoErr), apiClient.Close())
	}
	if err := validateEngineInfo(info.Info.OSType, info.Info.Architecture); err != nil {
		return nil, errors.Join(err, apiClient.Close())
	}
	filters := make(client.Filters).Add("label", managedLabel+"=true", hostLabel+"="+hostID)
	result, listErr := apiClient.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	closeErr := apiClient.Close()
	if listErr != nil {
		return nil, errors.Join(fmt.Errorf("list managed worker containers: %w", listErr), closeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close local Docker Engine client: %w", closeErr)
	}
	artifacts := make([]ArtifactMetadata, 0, len(result.Items))
	for _, item := range result.Items {
		artifacts = append(artifacts, metadataFromLabels(item.ID, item.Labels))
	}
	return artifacts, nil
}

func newLocalClient(controllerVersion string) (*client.Client, error) {
	return client.New(
		client.WithHost(localDockerHost),
		client.WithUserAgent("ci-runner/"+controllerVersion),
	)
}

func (r *Runtime) Close() error {
	r.cancel()
	r.wg.Wait()
	return r.close()
}

func (r *Runtime) List(ctx context.Context) ([]model.Worker, error) {
	if err := r.validateEngine(ctx); err != nil {
		return nil, err
	}
	filters := make(client.Filters).Add("label", managedLabel+"=true", hostLabel+"="+r.opts.HostID)
	result, err := r.engine.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list managed worker containers: %w", err)
	}
	workers := make([]model.Worker, 0, len(result.Items))
	adopted := make([]ArtifactMetadata, 0, len(result.Items))
	containerIDs := make([]string, 0, len(result.Items))
	for _, item := range result.Items {
		worker, inspectErr := r.workerFromContainer(ctx, item.ID, item.Labels, string(item.State), item.Created)
		if inspectErr != nil {
			r.opts.OnError(inspectErr)
			worker.State = model.WorkerStarting // unknown state is never assumed idle
		}
		workers = append(workers, worker)
		adopted = append(adopted, metadataFromLabels(item.ID, item.Labels))
		containerIDs = append(containerIDs, item.ID)
	}
	// Persist every active/exited managed container as adopted before retention
	// is allowed to inspect the catalog or any watcher can finalize a container.
	if err := r.opts.Artifacts.AdoptAndCleanup(ctx, adopted); err != nil {
		return nil, fmt.Errorf("adopt workers before artifact cleanup: %w", err)
	}
	for _, id := range containerIDs {
		r.ensureWatch(id, nil)
	}
	return workers, nil
}

func (r *Runtime) Start(ctx context.Context, request controller.StartWorkerRequest) (worker model.Worker, resultErr error) {
	telemetryStartedAt := time.Now()
	if request.ResourceTier == "" {
		request.ResourceTier = "default"
	}
	defer func() {
		r.opts.Telemetry.WorkerStarted(ctx, request.PoolID, request.ResourceTier, time.Since(telemetryStartedAt), telemetry.ClassifyWorkerStart(resultErr, controller.RunnerStartMayBeActive(resultErr)))
	}()
	if err := validateLimits(request); err != nil {
		return model.Worker{}, safeWorkerStartError(err)
	}
	if err := r.validateEngine(ctx); err != nil {
		return model.Worker{}, safeWorkerStartError(err)
	}
	if err := r.ensureImage(ctx); err != nil {
		return model.Worker{}, safeWorkerStartError(err)
	}
	jit := request.JITConfig.Reveal()
	defer clear(jit)
	runnerID := request.JITConfig.RunnerID()
	if runnerID <= 0 {
		return model.Worker{}, safeWorkerStartError(errors.New("JIT configuration is missing its positive GitHub runner ID"))
	}
	if len(jit) == 0 || bytes.ContainsAny(jit, "\x00\r\n") {
		return model.Worker{}, safeWorkerStartError(errors.New("JIT configuration must be a nonempty single line with no NUL byte"))
	}
	startedAt := time.Now().UTC()
	pids := request.Limits.PIDs
	create := client.ContainerCreateOptions{
		Name:     request.Name,
		Platform: &ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Config: &containertypes.Config{
			Image:       r.opts.Image,
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Env: []string{
				"ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1",
			},
			Labels: map[string]string{
				managedLabel: "true", hostLabel: r.opts.HostID, poolLabel: request.PoolID,
				workerNameLabel: request.Name, resourceTierLabel: request.ResourceTier, runnerIDLabel: strconv.FormatInt(runnerID, 10), startedAtLabel: startedAt.Format(time.RFC3339Nano), controllerLabel: r.opts.ControllerVersion,
				composeProjectLabel: "ci-runner-" + r.opts.HostID, composeServiceLabel: composeServiceName,
				composeNumberLabel: strconv.FormatInt(runnerID, 10), composeOneoffLabel: "False",
			},
		},
		HostConfig: &containertypes.HostConfig{
			Binds: nil, Mounts: nil, VolumesFrom: nil, Privileged: false, AutoRemove: false,
			NetworkMode: "bridge", PublishAllPorts: false,
			RestartPolicy: containertypes.RestartPolicy{Name: containertypes.RestartPolicyDisabled},
			LogConfig: containertypes.LogConfig{Type: "local", Config: map[string]string{
				"max-size": strconv.FormatUint(r.opts.DockerLogMaxSizeBytes, 10),
				"max-file": strconv.Itoa(r.opts.DockerLogMaxFiles),
			}},
			Resources: containertypes.Resources{
				NanoCPUs: int64(request.Limits.CPUs * 1_000_000_000), Memory: int64(request.Limits.Memory),
				MemorySwap: int64(request.Limits.MemorySwap), PidsLimit: &pids,
				Devices: nil, DeviceRequests: nil, DeviceCgroupRules: nil,
			},
		},
	}
	created, err := r.engine.ContainerCreate(ctx, create)
	if err != nil {
		return model.Worker{}, safeWorkerStartError(fmt.Errorf("create worker container: %w", err))
	}
	if len(created.Warnings) > 0 {
		_, _ = r.engine.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return model.Worker{}, safeWorkerStartError(fmt.Errorf("worker configuration was rejected by Docker: %s", strings.Join(created.Warnings, "; ")))
	}
	attached, err := r.engine.ContainerAttach(ctx, created.ID, client.ContainerAttachOptions{Stream: true, Stdin: true})
	if err != nil {
		_, _ = r.engine.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		return model.Worker{}, safeWorkerStartError(fmt.Errorf("attach JIT configuration stream: %w", err))
	}
	attachedClosed := false
	defer func() {
		if !attachedClosed {
			attached.Close()
		}
	}()
	wait := r.engine.ContainerWait(r.ctx, created.ID, client.ContainerWaitOptions{Condition: containertypes.WaitConditionNextExit})
	if _, err := r.engine.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		attached.Close()
		attachedClosed = true
		// ContainerStart is not transactional. A response error may arrive after
		// the engine started the process, so force-removal here could cancel a job.
		// Adopt/watch the ambiguous container and let inventory reconcile it.
		r.ensureWatch(created.ID, &wait)
		return model.Worker{}, &controller.WorkerStartError{Err: fmt.Errorf("start worker container: %w", err), RunnerMayBeActive: true}
	}
	r.ensureWatch(created.ID, &wait)
	if err := writeJITConfiguration(attached.Conn, jit); err != nil {
		attached.Close()
		attachedClosed = true
		// The runner may have received a complete line before the stream reported
		// an error. Never force-remove an ambiguity after ContainerStart.
		return model.Worker{}, &controller.WorkerStartError{Err: err, RunnerMayBeActive: true}
	}
	if err := attached.CloseWrite(); err != nil {
		attached.Close()
		attachedClosed = true
		// CloseWrite may fail after EOF reached the entrypoint. The container is
		// already watched and must be treated as potentially assigned.
		return model.Worker{}, &controller.WorkerStartError{Err: fmt.Errorf("close JIT configuration stream: %w", err), RunnerMayBeActive: true}
	}
	attached.Close()
	attachedClosed = true
	return model.Worker{ID: created.ID, AdapterID: created.ID, PoolID: request.PoolID, Name: request.Name, RunnerID: runnerID, State: model.WorkerStarting, StartedAt: startedAt}, nil
}

func (r *Runtime) validateEngine(ctx context.Context) error {
	result, err := r.engine.Info(ctx, client.InfoOptions{})
	if err != nil {
		return fmt.Errorf("inspect local Docker Engine: %w", err)
	}
	return validateEngineInfo(result.Info.OSType, result.Info.Architecture)
}

func validateEngineInfo(operatingSystem, architecture string) error {
	if operatingSystem != "linux" || architecture != "x86_64" && architecture != "amd64" {
		return fmt.Errorf("local Docker Engine must be linux/amd64, got %s/%s", operatingSystem, architecture)
	}
	return nil
}

func writeJITConfiguration(destination io.Writer, jit []byte) error {
	if written, err := destination.Write(jit); err != nil {
		return fmt.Errorf("stream JIT configuration: %w", err)
	} else if written != len(jit) {
		return fmt.Errorf("stream JIT configuration: %w", io.ErrShortWrite)
	}
	if _, err := destination.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("terminate JIT configuration stream: %w", err)
	}
	return nil
}

func safeWorkerStartError(err error) error {
	return &controller.WorkerStartError{Err: err, RunnerMayBeActive: false}
}

func (r *Runtime) RemoveIfIdle(ctx context.Context, id string) (bool, error) {
	first, err := r.readWorkerState(ctx, id)
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	if err != nil || first != "idle" {
		return false, err
	}
	timer := time.NewTimer(r.opts.IdleConfirmationWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
	}
	second, err := r.readWorkerState(ctx, id)
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	if err != nil || second != "idle" {
		return false, err
	}
	watch := r.ensureWatch(id, nil)
	neverForce := -1
	if _, err := r.engine.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &neverForce}); err != nil && !cerrdefs.IsNotFound(err) {
		return false, fmt.Errorf("gracefully stop idle worker: %w", err)
	}
	if err := waitForWatch(ctx, watch); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Runtime) ForceStop(ctx context.Context, id string) error {
	watch := r.ensureWatch(id, nil)
	if _, err := r.engine.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "SIGKILL"}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("kill worker container: %w", err)
	}
	return waitForWatch(ctx, watch)
}

func (r *Runtime) ensureImage(ctx context.Context) error {
	if _, err := r.engine.ImageInspect(ctx, r.opts.Image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect pinned worker image: %w", err)
	}
	// A stalled or hung registry pull otherwise has no independent stall
	// detector anywhere in this call chain: it would sit unresponsive under
	// whatever deadline the caller passed in (the reconcile Step watchdog,
	// sized generously to never cut off a legitimate Step) instead of failing
	// fast the way GitHub's own scale-set transport does. Bound it with its
	// own configured timeout, mirroring how ControllerDesktopAdapter.Start/Stop
	// bound Docker Desktop's lifecycle with cfg.DockerDesktop.StartTimeout/StopTimeout.
	pullCtx, cancel := context.WithTimeout(ctx, r.opts.ImagePullTimeout)
	defer cancel()
	pull, err := r.engine.ImagePull(pullCtx, r.opts.Image, client.ImagePullOptions{Platforms: []ocispec.Platform{{OS: "linux", Architecture: "amd64"}}})
	if err != nil {
		return fmt.Errorf("pull pinned worker image: %w", err)
	}
	waitErr := pull.Wait(pullCtx)
	closeErr := pull.Close()
	if waitErr != nil {
		return errors.Join(
			fmt.Errorf("pull pinned worker image: %w", waitErr),
			wrapIfError("close pinned worker image pull", closeErr),
		)
	}
	return wrapIfError("close pinned worker image pull", closeErr)
}

func (r *Runtime) workerFromContainer(ctx context.Context, id string, labels map[string]string, containerState string, created int64) (model.Worker, error) {
	started := time.Unix(created, 0).UTC()
	if parsed, err := time.Parse(time.RFC3339Nano, labels[startedAtLabel]); err == nil {
		started = parsed
	}
	worker := model.Worker{
		ID: id, AdapterID: id, PoolID: labels[poolLabel], Name: labels[workerNameLabel],
		State: model.WorkerStarting, StartedAt: started,
	}
	worker.RunnerID, _ = strconv.ParseInt(labels[runnerIDLabel], 10, 64)
	if containerState != "running" {
		worker.State = model.WorkerExited
		return worker, nil
	}
	state, err := r.readWorkerState(ctx, id)
	if err != nil {
		return worker, fmt.Errorf("read worker %s state: %w", id, err)
	}
	switch state {
	case "idle":
		worker.State = model.WorkerIdle
	case "busy", "completed":
		worker.State = model.WorkerBusy
	default:
		return worker, fmt.Errorf("worker %s returned unsupported state %q", id, state)
	}
	return worker, nil
}

func (r *Runtime) readWorkerState(ctx context.Context, id string) (_ string, resultErr error) {
	result, err := r.engine.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: r.opts.StatePath})
	if err != nil {
		return "", err
	}
	defer func() {
		resultErr = errors.Join(resultErr, wrapIfError("close worker state archive", result.Content.Close()))
	}()
	tarReader := tar.NewReader(io.LimitReader(result.Content, 64*1024))
	header, err := tarReader.Next()
	if err != nil {
		return "", fmt.Errorf("read state archive: %w", err)
	}
	if header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maximumStateBytes {
		return "", errors.New("worker state file has unsafe type or size")
	}
	content, err := io.ReadAll(io.LimitReader(tarReader, maximumStateBytes+1))
	if err != nil {
		return "", err
	}
	state := string(content)
	if state != "idle" && state != "busy" && state != "completed" {
		return "", fmt.Errorf("invalid worker state %q", state)
	}
	return state, nil
}

func (r *Runtime) ensureWatch(id string, existing *client.ContainerWaitResult) *containerWatch {
	r.mu.Lock()
	if watch, ok := r.watches[id]; ok {
		r.mu.Unlock()
		return watch
	}
	watch := &containerWatch{done: make(chan struct{})}
	r.watches[id] = watch
	r.wg.Add(1)
	r.mu.Unlock()

	wait := existing
	if wait == nil {
		created := r.engine.ContainerWait(r.ctx, id, client.ContainerWaitOptions{Condition: containertypes.WaitConditionNotRunning})
		wait = &created
	}
	go r.finalize(id, wait, watch)
	return watch
}

func (r *Runtime) finalize(id string, wait *client.ContainerWaitResult, watch *containerWatch) {
	defer r.wg.Done()
	telemetryStartedAt := time.Now()
	telemetryMetadata := r.metadata(r.ctx, id)
	finalization := telemetry.WorkerFinalization{ResourceTier: telemetryMetadata.ResourceTier}
	defer func() {
		finalization.Err = watch.err
		if shutdownErr := r.ctx.Err(); shutdownErr != nil && errorCausedOnlyBy(watch.err, shutdownErr) {
			finalization.ControllerShutdown = true
		}
		finalization.Duration = time.Since(telemetryStartedAt)
		r.opts.Telemetry.WorkerFinalized(context.Background(), telemetryMetadata.PoolID, finalization)

		// The watch becoming done is an externally observable lifecycle boundary.
		// Publish telemetry first so callers cannot observe completion before the
		// corresponding finalization signal exists.
		r.mu.Lock()
		delete(r.watches, id)
		close(watch.done)
		r.mu.Unlock()
	}()
	logContext, cancelLogs := context.WithCancel(r.ctx)
	defer cancelLogs()
	logDone := make(chan error, 1)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		logDone <- r.captureLogs(logContext, id)
	}()

	var waitErr error
	resultChannel := wait.Result
	errorChannel := wait.Error
	for resultChannel != nil || errorChannel != nil {
		select {
		case <-r.ctx.Done():
			finalization.ControllerShutdown = true
			watch.err = r.ctx.Err()
			return
		case result, ok := <-resultChannel:
			if ok {
				finalization.ExitCode = result.StatusCode
				finalization.ExitObserved = true
				resultChannel = nil
				errorChannel = nil
			} else {
				resultChannel = nil
			}
		case err, ok := <-errorChannel:
			if ok && err != nil {
				waitErr = err
				resultChannel = nil
				errorChannel = nil
			} else if !ok {
				errorChannel = nil
			}
		}
	}
	if waitErr != nil {
		inspect, err := r.engine.ContainerInspect(r.ctx, id, client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			// A concurrent Docker cleanup can remove a stopped ephemeral worker
			// between inventory and wait. This is bounded missing lifecycle
			// evidence, not an adapter failure and not an inferred zero peak.
			finalization.ResourceEvidence = &telemetry.WorkerResourceEvidence{Status: "missing"}
			finalization.RecordResourceEvidence = true
			return
		}
		if err != nil || inspect.Container.State == nil || inspect.Container.State.Running {
			watch.err = fmt.Errorf("wait for worker exit: %w", waitErr)
			r.opts.OnError(watch.err)
			return
		}
		finalization.ExitCode = int64(inspect.Container.State.ExitCode)
		finalization.ExitObserved = true
	}

	finalizeCtx, cancel := context.WithTimeout(r.ctx, r.opts.FinalizationTimeout)
	defer cancel()
	select {
	case err := <-logDone:
		cancelLogs()
		if err != nil {
			r.opts.OnError(err)
			watch.err = errors.Join(watch.err, err)
		}
	case <-finalizeCtx.Done():
		err := errors.New("worker log stream did not close before finalization timeout")
		if shutdownErr := r.ctx.Err(); shutdownErr != nil {
			err = fmt.Errorf("worker log stream canceled by controller shutdown: %w", shutdownErr)
		}
		r.opts.OnError(err)
		watch.err = errors.Join(watch.err, err)
		cancelLogs()
		// Do not delete the watch while its log writer is still active. Keeping
		// the watch registered prevents List from starting concurrent retries
		// against the same artifact path.
		if logErr := <-logDone; logErr != nil {
			shutdownErr := r.ctx.Err()
			if shutdownErr == nil || !errorCausedOnlyBy(logErr, shutdownErr) {
				r.opts.OnError(logErr)
				watch.err = errors.Join(watch.err, logErr)
			}
		}
	}
	if err := r.captureDiagnostics(finalizeCtx, id); err != nil {
		r.opts.OnError(err)
		watch.err = errors.Join(watch.err, err)
	}
	metadata := r.metadata(finalizeCtx, id)
	evidence, evidenceRecorded, evidenceErr := r.captureResourceEvidence(finalizeCtx, id, metadata)
	if evidenceErr != nil {
		r.opts.OnError(evidenceErr)
		watch.err = errors.Join(watch.err, evidenceErr)
	}
	finalization.ResourceTier = metadata.ResourceTier
	finalization.RecordResourceEvidence = evidenceRecorded
	finalization.ResourceEvidence = &telemetry.WorkerResourceEvidence{
		Status: evidence.Status, Missing: append([]string(nil), evidence.Missing...),
		MemoryPeakBytes:          evidence.Memory.PeakBytes,
		MemorySwapPeakBytes:      evidence.Memory.SwapPeakBytes,
		OOMEvents:                evidence.Memory.OOMEvents,
		OOMKillEvents:            evidence.Memory.OOMKillEvents,
		CPUPeriods:               evidence.CPU.Periods,
		CPUThrottledPeriods:      evidence.CPU.ThrottledPeriods,
		CPUThrottledMicroseconds: evidence.CPU.ThrottledMicroseconds,
		PIDsPeak:                 evidence.PIDs.Peak,
		IOReadBytes:              evidence.IO.ReadBytes,
		IOWriteBytes:             evidence.IO.WriteBytes,
	}
	if watch.err == nil {
		if err := r.opts.Artifacts.Finalize(finalizeCtx, metadata); err != nil {
			err = fmt.Errorf("finalize worker artifacts: %w", err)
			r.opts.OnError(err)
			watch.err = err
		}
	}
	if watch.err != nil {
		// The exited container is the durable retry source for logs and _diag.
		// List adopts it again after restart, then a fresh watcher retries.
		return
	}
	if _, err := r.engine.ContainerRemove(finalizeCtx, id, client.ContainerRemoveOptions{RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
		err = fmt.Errorf("remove completed worker container: %w", err)
		r.opts.OnError(err)
		watch.err = errors.Join(watch.err, err)
	}
}

func (r *Runtime) captureLogs(ctx context.Context, id string) error {
	metadata := r.metadata(ctx, id)
	destination, err := r.opts.Artifacts.OpenLog(ctx, metadata)
	if err != nil {
		return err
	}
	logs, err := r.engine.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true, Timestamps: true})
	if err != nil {
		return errors.Join(fmt.Errorf("open worker log stream: %w", err), destination.Close())
	}
	copyDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = logs.Close()
		case <-copyDone:
		}
	}()
	_, copyErr := stdcopy.StdCopy(destination, destination, logs)
	close(copyDone)
	return errors.Join(
		wrapIfError("copy worker log stream", copyErr),
		wrapIfError("close worker log stream", logs.Close()),
		wrapIfError("close worker log artifact", destination.Close()),
	)
}

func (r *Runtime) captureDiagnostics(ctx context.Context, id string) (resultErr error) {
	result, err := r.engine.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: r.opts.DiagnosticPath})
	if err != nil {
		return fmt.Errorf("copy worker diagnostics: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, wrapIfError("close worker diagnostics archive", result.Content.Close()))
	}()
	if err := r.opts.Artifacts.WriteDiagnostics(ctx, r.metadata(ctx, id), result.Content); err != nil {
		return err
	}
	return nil
}

func (r *Runtime) captureResourceEvidence(ctx context.Context, id string, metadata ArtifactMetadata) (ResourceEvidence, bool, error) {
	evidence := fallbackResourceEvidence("unavailable", "docker-copy-unavailable")
	if err := ctx.Err(); err != nil {
		// A finalization timeout is an attempt failure, not evidence that Docker
		// or cgroup data is unavailable. Do not publish an immutable fallback;
		// the retained container can provide real terminal evidence on retry.
		return evidence, false, fmt.Errorf("terminal worker resource evidence context expired: %w", err)
	}
	result, err := r.engine.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: r.opts.ResourceEvidencePath})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return evidence, false, fmt.Errorf("copy terminal worker resource evidence after finalization context expired: %w", errors.Join(err, contextErr))
		}
		if shutdownErr := r.ctx.Err(); shutdownErr != nil && errorCausedOnlyBy(err, shutdownErr) {
			return evidence, false, fmt.Errorf("copy terminal worker resource evidence: %w", err)
		}
		r.opts.OnError(fmt.Errorf("copy terminal worker resource evidence; preserving bounded fallback: %w", err))
	} else {
		content, extractErr := readSingleRegularArchive(result.Content, maximumResourceEvidenceBytes)
		closeErr := result.Content.Close()
		if err := errors.Join(extractErr, closeErr); err != nil {
			r.opts.OnError(fmt.Errorf("extract terminal worker resource evidence; preserving bounded fallback: %w", err))
			evidence = fallbackResourceEvidence("invalid", "invalid-evidence")
		} else if parsed, parseErr := ParseResourceEvidence(strings.NewReader(string(content))); parseErr != nil {
			r.opts.OnError(fmt.Errorf("validate terminal worker resource evidence; preserving bounded fallback: %w", parseErr))
			evidence = fallbackResourceEvidence("invalid", "invalid-evidence")
		} else {
			evidence = parsed
		}
	}
	if err := ctx.Err(); err != nil && evidence.Source == resourceEvidenceSourceHost {
		return evidence, false, fmt.Errorf("terminal worker fallback evidence context expired before persistence: %w", err)
	}
	recorded, err := r.opts.Artifacts.WriteResourceEvidence(ctx, metadata, evidence)
	if err != nil {
		return evidence, false, fmt.Errorf("persist terminal worker resource evidence: %w", err)
	}
	return evidence, recorded, nil
}

func readSingleRegularArchive(source io.Reader, maximum int64) ([]byte, error) {
	tarReader := tar.NewReader(io.LimitReader(source, maximum+64*1024))
	header, err := tarReader.Next()
	if err != nil {
		return nil, err
	}
	if header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maximum {
		return nil, errors.New("resource evidence archive must contain one bounded regular file")
	}
	content, err := io.ReadAll(io.LimitReader(tarReader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != header.Size {
		return nil, errors.New("resource evidence archive length does not match its header")
	}
	if _, err := tarReader.Next(); !errors.Is(err, io.EOF) {
		return nil, errors.New("resource evidence archive must contain exactly one file")
	}
	return content, nil
}

func (r *Runtime) metadata(ctx context.Context, id string) ArtifactMetadata {
	metadata := ArtifactMetadata{ContainerID: id}
	inspect, err := r.engine.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.Config == nil {
		return metadata
	}
	return metadataFromLabels(id, inspect.Container.Config.Labels)
}

func metadataFromLabels(id string, labels map[string]string) ArtifactMetadata {
	resourceTier := labels[resourceTierLabel]
	if resourceTier == "" {
		resourceTier = "unknown"
	}
	metadata := ArtifactMetadata{ContainerID: id, WorkerName: labels[workerNameLabel], PoolID: labels[poolLabel], ResourceTier: resourceTier}
	metadata.StartedAt, _ = time.Parse(time.RFC3339Nano, labels[startedAtLabel])
	return metadata
}

func wrapIfError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// errorCausedOnlyBy recognizes wrapping and errors.Join while refusing to
// hide any unrelated artifact/runtime failure that happened alongside a
// controller shutdown.
func errorCausedOnlyBy(err, cause error) bool {
	if err == nil || cause == nil {
		return false
	}
	if err == cause {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		found := false
		for _, child := range children {
			if child == nil {
				continue
			}
			found = true
			if !errorCausedOnlyBy(child, cause) {
				return false
			}
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return errorCausedOnlyBy(wrapped.Unwrap(), cause)
	}
	return errors.Is(err, cause)
}

func waitForWatch(ctx context.Context, watch *containerWatch) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-watch.done:
		return watch.err
	}
}

func validateLimits(request controller.StartWorkerRequest) error {
	limits := request.Limits
	if request.PoolID == "" || request.Name == "" {
		return errors.New("worker pool and name are required")
	}
	if request.ResourceTier != "default" && request.ResourceTier != "target_override" && request.ResourceTier != "unknown" {
		return errors.New("worker resource tier must be default, target_override, or unknown")
	}
	if limits.CPUs <= 0 || limits.Memory == 0 || limits.MemorySwap < limits.Memory || limits.PIDs <= 0 {
		return errors.New("worker CPU, memory, total memory+swap, and PID limits are invalid")
	}
	if limits.CPUs > float64(^uint64(0)>>1)/1_000_000_000 || uint64(limits.Memory) > uint64(^uint64(0)>>1) || uint64(limits.MemorySwap) > uint64(^uint64(0)>>1) {
		return errors.New("worker resource limit exceeds Docker's signed integer range")
	}
	return nil
}

var _ controller.WorkerRuntime = (*Runtime)(nil)
var _ controller.ForceStopRuntime = (*Runtime)(nil)
