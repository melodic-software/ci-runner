package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"iter"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/scaleset"
	"github.com/melodic-software/ci-runner/internal/telemetry"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

const pinnedTestImage = "ghcr.io/actions/actions-runner:2.335.1@sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29"

func TestStartCreatesConstrainedSecretMinimalContainer(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	sink := &memoryArtifacts{}
	runtime := newTestRuntime(t, engine, sink)
	defer runtime.Close()
	request := controller.StartWorkerRequest{
		PoolID: "org", Name: "melo-desk-001-worker", JITConfig: scaleset.NewRunnerJITConfig([]byte("test-jit"), 99),
		Limits: config.Worker{CPUs: 2, Memory: config.ByteSize(8 << 30), MemorySwap: config.ByteSize(8 << 30), PIDs: 4096},
	}
	worker, err := runtime.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if worker.State != model.WorkerStarting || worker.PoolID != "org" || worker.RunnerID != 99 {
		t.Fatalf("worker = %#v", worker)
	}
	created := engine.createOptions()
	if created.Config == nil || created.HostConfig == nil {
		t.Fatal("missing container or host configuration")
	}
	if created.Config.Image != pinnedTestImage || created.Platform == nil || created.Platform.OS != "linux" || created.Platform.Architecture != "amd64" {
		t.Fatalf("image/platform = %q %#v", created.Config.Image, created.Platform)
	}
	if got := created.Config.Env; len(got) != 1 || got[0] != "ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1" {
		t.Fatalf("environment = %#v", got)
	}
	if !created.Config.AttachStdin || !created.Config.OpenStdin || !created.Config.StdinOnce {
		t.Fatalf("secret stdin configuration = %#v", created.Config)
	}
	if got := engine.attachedInput(); got != "test-jit\n" {
		t.Fatalf("attached input = %q", got)
	}
	if created.Config.Labels[runnerIDLabel] != "99" {
		t.Fatalf("GitHub runner ID label = %q", created.Config.Labels[runnerIDLabel])
	}
	wantComposeLabels := map[string]string{
		composeProjectLabel: "ci-runner-melo-desk-001",
		composeServiceLabel: composeServiceName,
		composeNumberLabel:  "99",
		composeOneoffLabel:  "False",
	}
	for label, want := range wantComposeLabels {
		if got := created.Config.Labels[label]; got != want {
			t.Errorf("Docker Desktop project label %q = %q, want %q", label, got, want)
		}
	}
	for _, value := range created.Config.Env {
		if strings.Contains(value, "PRIVATE_KEY") || strings.Contains(value, "INSTALLATION_TOKEN") || strings.Contains(value, "DOCKER_HOST") {
			t.Fatalf("unexpected credential or host control in worker env: %q", value)
		}
	}
	host := created.HostConfig
	if host.Privileged || host.AutoRemove || host.PublishAllPorts || len(host.Binds) != 0 || len(host.Mounts) != 0 || len(host.VolumesFrom) != 0 {
		t.Fatalf("unsafe host configuration = %#v", host)
	}
	if len(host.Resources.Devices) != 0 || len(host.Resources.DeviceRequests) != 0 || len(host.Resources.DeviceCgroupRules) != 0 {
		t.Fatalf("worker received devices: %#v", host.Resources)
	}
	if host.Resources.NanoCPUs != 2_000_000_000 || host.Resources.Memory != 8<<30 || host.Resources.MemorySwap != 8<<30 || host.Resources.PidsLimit == nil || *host.Resources.PidsLimit != 4096 {
		t.Fatalf("resource limits = %#v", host.Resources)
	}
	if host.LogConfig.Type != "local" || host.LogConfig.Config["max-size"] != "10485760" || host.LogConfig.Config["max-file"] != "3" {
		t.Fatalf("log config = %#v", host.LogConfig)
	}
	if host.RestartPolicy.Name != containertypes.RestartPolicyDisabled {
		t.Fatalf("restart policy = %#v", host.RestartPolicy)
	}
	if len(created.Config.Volumes) != 0 || len(created.Config.ExposedPorts) != 0 {
		t.Fatal("worker image exposed persistent volumes or ports")
	}
	if engine.pullCount() != 0 {
		t.Fatal("cached pinned image was pulled unnecessarily")
	}
}

func TestStartPullsOnlyWhenPinnedImageMissing(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.imagePresent = false
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	_, err := runtime.Start(context.Background(), controller.StartWorkerRequest{
		PoolID: "org", Name: "worker", JITConfig: scaleset.NewRunnerJITConfig([]byte("jit"), 99),
		Limits: config.Worker{CPUs: 1, Memory: 1 << 30, MemorySwap: 1 << 30, PIDs: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	if engine.pullCount() != 1 || engine.pulledImage() != pinnedTestImage {
		t.Fatalf("pull count/image = %d %q", engine.pullCount(), engine.pulledImage())
	}
}

func TestRuntimeEmitsBoundedWorkerLifecycleTelemetry(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	recorder := &recordingTelemetry{}
	options := testOptions(&memoryArtifacts{})
	options.Telemetry = recorder
	runtime, err := New(engine, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	worker, err := runtime.Start(context.Background(), controller.StartWorkerRequest{
		PoolID: "org", Name: "worker", JITConfig: scaleset.NewRunnerJITConfig([]byte("jit"), 99),
		Limits: config.Worker{CPUs: 1, Memory: 1 << 30, MemorySwap: 1 << 30, PIDs: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	watch := runtime.watchForTest(worker.ID)
	engine.signalExit(worker.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	starts, finalizations := recorder.snapshot()
	if len(starts) != 1 || starts[0].poolID != "org" || starts[0].outcome != telemetry.WorkerStartSucceeded {
		t.Fatalf("worker starts = %#v", starts)
	}
	if len(finalizations) != 1 || finalizations[0].poolID != "org" || telemetry.ClassifyWorkerFinalization(finalizations[0].value) != telemetry.WorkerFinalizationCompleted {
		t.Fatalf("worker finalizations = %#v", finalizations)
	}
}

func TestResourceEvidenceCopyFailurePersistsFallbackWithoutBlockingDiagnostics(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.resourceCopyErr = errors.New("Docker archive endpoint unavailable")
	sink := &memoryArtifacts{}
	recorder := &recordingTelemetry{}
	options := testOptions(sink)
	options.Telemetry = recorder
	runtime, err := New(engine, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	worker, err := runtime.Start(context.Background(), controller.StartWorkerRequest{
		PoolID: "org", Name: "worker", ResourceTier: "target_override",
		JITConfig: scaleset.NewRunnerJITConfig([]byte("jit"), 99),
		Limits:    config.Worker{CPUs: 1, Memory: 1 << 30, MemorySwap: 1 << 30, PIDs: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	watch := runtime.watchForTest(worker.ID)
	engine.signalExit(worker.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForWatch(ctx, watch); err != nil {
		t.Fatalf("bounded resource fallback blocked finalization: %v", err)
	}
	if engine.hasContainer(worker.ID) {
		t.Fatal("successfully diagnosed worker was retained after resource fallback")
	}
	sink.mu.Lock()
	if len(sink.resources) != 1 || sink.resources[0].Status != "unavailable" || sink.resources[0].Reason != "docker-copy-unavailable" {
		t.Fatalf("resource fallbacks = %#v", sink.resources)
	}
	gotEvents := append([]string(nil), sink.events...)
	sink.mu.Unlock()
	for _, event := range []string{"open-log", "diagnostics", "resources", "finalize"} {
		if !containsCall(gotEvents, event) {
			t.Fatalf("artifact event %q missing from %v", event, gotEvents)
		}
	}
	_, finalizations := recorder.snapshot()
	if len(finalizations) != 1 || finalizations[0].value.ResourceTier != "target_override" || finalizations[0].value.ResourceEvidence == nil || finalizations[0].value.ResourceEvidence.Status != "unavailable" {
		t.Fatalf("fallback telemetry = %#v", finalizations)
	}
}

func TestStartRejectsMultilineJITAndNonLinuxEngine(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	request := controller.StartWorkerRequest{
		PoolID: "org", Name: "worker", JITConfig: scaleset.NewRunnerJITConfig([]byte("line-one\nline-two"), 99),
		Limits: config.Worker{CPUs: 1, Memory: 1 << 30, MemorySwap: 1 << 30, PIDs: 128},
	}
	if _, err := runtime.Start(context.Background(), request); err == nil {
		t.Fatal("multiline JIT configuration accepted")
	}

	request.JITConfig = scaleset.NewRunnerJITConfig([]byte("jit"), 99)
	engine.setPlatform("windows", "amd64")
	if _, err := runtime.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("non-Linux engine error = %v", err)
	}
}

func TestStartNeverForceRemovesAfterAmbiguousJITDelivery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		writeErrorAt  int
		closeWriteErr error
	}{
		{name: "newline-write-returned-bytes-and-error", writeErrorAt: 2},
		{name: "close-write-error-after-complete-line", closeWriteErr: errors.New("close-write response lost")},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			engine := newFakeEngine()
			engine.attachWriteErrorAt = test.writeErrorAt
			engine.attachCloseWriteErr = test.closeWriteErr
			runtime := newTestRuntime(t, engine, &memoryArtifacts{})
			defer runtime.Close()
			_, err := runtime.Start(context.Background(), controller.StartWorkerRequest{
				PoolID: "org", Name: "worker", JITConfig: scaleset.NewRunnerJITConfig([]byte("jit"), 99),
				Limits: config.Worker{CPUs: 1, Memory: 1 << 30, MemorySwap: 1 << 30, PIDs: 128},
			})
			if err == nil {
				t.Fatal("expected ambiguous stream error")
			}
			if !engine.hasContainer("created-container") || containsCall(engine.callsSnapshot(), "remove:created-container") {
				t.Fatalf("ambiguous started container was force-removed: %v", engine.callsSnapshot())
			}
		})
	}
}

func TestNewLocalIgnoresDockerEnvironmentOverrides(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://attacker.example:2375")
	t.Setenv("DOCKER_API_VERSION", "1.24")
	t.Setenv("DOCKER_CERT_PATH", t.TempDir())
	runtime, err := NewLocal(testOptions(&memoryArtifacts{}))
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	apiClient, ok := runtime.engine.(*client.Client)
	if !ok {
		t.Fatalf("engine type = %T", runtime.engine)
	}
	if got := apiClient.DaemonHost(); got != localDockerHost {
		t.Fatalf("Docker host = %q, want fixed local endpoint %q", got, localDockerHost)
	}
}

func TestRemoveIfIdleRequiresTwoIdleObservationsAndNeverForces(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("idle-worker", "idle", "running")
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	removed, err := runtime.RemoveIfIdle(context.Background(), "idle-worker")
	if err != nil || !removed {
		t.Fatalf("removed=%v error=%v", removed, err)
	}
	if engine.stopCount() != 1 || engine.killCount() != 0 {
		t.Fatalf("normal drain stop=%d kill=%d", engine.stopCount(), engine.killCount())
	}
	if timeout := engine.lastStopTimeout(); timeout == nil || *timeout != -1 {
		t.Fatalf("normal stop timeout = %#v, want -1 (never force)", timeout)
	}
	assertCallBefore(t, engine.callsSnapshot(), "copy:/home/runner/_diag", "remove:idle-worker")
}

func TestRemoveIfIdlePreservesBusyOrChangingWorker(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("busy-worker", "busy", "running")
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	removed, err := runtime.RemoveIfIdle(context.Background(), "busy-worker")
	if err != nil || removed {
		t.Fatalf("removed=%v error=%v", removed, err)
	}
	if engine.stopCount() != 0 || engine.killCount() != 0 {
		t.Fatal("busy worker received a stop signal")
	}

	engine.addContainer("racing-worker", "idle", "running")
	engine.changeStateAfterReads("racing-worker", 1, "busy")
	removed, err = runtime.RemoveIfIdle(context.Background(), "racing-worker")
	if err != nil || removed {
		t.Fatalf("racing removed=%v error=%v", removed, err)
	}
	if engine.stopCount() != 0 || engine.killCount() != 0 {
		t.Fatal("worker that changed to busy received a stop signal")
	}
}

func TestForceStopUsesExplicitKillThenPreservesDiagnostics(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("busy-worker", "busy", "running")
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	if err := runtime.ForceStop(context.Background(), "busy-worker"); err != nil {
		t.Fatal(err)
	}
	if engine.killCount() != 1 || engine.stopCount() != 0 {
		t.Fatalf("force path kill=%d stop=%d", engine.killCount(), engine.stopCount())
	}
	assertCallBefore(t, engine.callsSnapshot(), "copy:/home/runner/_diag", "remove:busy-worker")
}

func TestListReconstructsStateFromOfficialHookFile(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("idle", "idle", "running")
	engine.addContainer("busy", "busy", "running")
	engine.addContainer("complete", "completed", "running")
	engine.addContainer("exited", "completed", "exited")
	runtime := newTestRuntime(t, engine, &memoryArtifacts{})
	defer runtime.Close()
	workers, err := runtime.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]model.WorkerState{}
	for _, worker := range workers {
		states[worker.ID] = worker.State
	}
	if states["idle"] != model.WorkerIdle || states["busy"] != model.WorkerBusy || states["complete"] != model.WorkerBusy || states["exited"] != model.WorkerExited {
		t.Fatalf("states = %#v", states)
	}
}

func TestListAdoptsAllContainersBeforeStartingFinalizers(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("exited", "completed", "exited")
	sink := &memoryArtifacts{}
	runtime := newTestRuntime(t, engine, sink)
	defer runtime.Close()
	if _, err := runtime.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) == 0 || sink.events[0] != "adopt" || len(sink.adopted[0]) != 1 || sink.adopted[0][0].ContainerID != "exited" {
		t.Fatalf("artifact lifecycle events = %v adopted=%#v", sink.events, sink.adopted)
	}
}

func TestArtifactPersistenceFailureRetainsExitedContainerAndListRetries(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.addContainer("retry-worker", "completed", "exited")
	sink := &memoryArtifacts{finalizeErr: errors.New("durable index unavailable")}
	runtime := newTestRuntime(t, engine, sink)
	defer runtime.Close()
	if _, err := runtime.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstWatch := runtime.watchForTest("retry-worker")
	engine.signalExit("retry-worker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForWatch(ctx, firstWatch); err == nil || !strings.Contains(err.Error(), "durable index unavailable") {
		t.Fatalf("first finalization error = %v", err)
	}
	if !engine.hasContainer("retry-worker") {
		t.Fatal("artifact failure deleted the only diagnostic retry source")
	}

	sink.mu.Lock()
	sink.finalizeErr = nil
	sink.mu.Unlock()
	if _, err := runtime.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	secondWatch := runtime.watchForTest("retry-worker")
	engine.signalExit("retry-worker")
	if err := waitForWatch(ctx, secondWatch); err != nil {
		t.Fatalf("retry finalization: %v", err)
	}
	if engine.hasContainer("retry-worker") {
		t.Fatal("successfully persisted exited container was not removed")
	}
}

func TestStuckLogStreamIsCanceledBeforeWatchRetry(t *testing.T) {
	t.Parallel()
	engine := newFakeEngine()
	engine.blockLogs = true
	engine.addContainer("stuck-worker", "completed", "running")
	options := testOptions(&memoryArtifacts{})
	options.FinalizationTimeout = 10 * time.Millisecond
	runtime, err := New(engine, options)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := runtime.watchForTest("stuck-worker")
	engine.signalExit("stuck-worker")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForWatch(ctx, first); err == nil || !strings.Contains(err.Error(), "log stream did not close") {
		t.Fatalf("first finalization error = %v", err)
	}
	if active, maximum := engine.logStreamCounts(); active != 0 || maximum != 1 {
		t.Fatalf("log streams after timeout active=%d max=%d", active, maximum)
	}
	if _, err := runtime.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		active, maximum := engine.logStreamCounts()
		if active == 1 {
			if maximum != 1 {
				t.Fatalf("retry overlapped prior log writer active=%d max=%d", active, maximum)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("retry log writer did not start; active=%d max=%d", active, maximum)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNewRejectsMutableImageAndMissingArtifactSink(t *testing.T) {
	t.Parallel()
	base := testOptions(&memoryArtifacts{})
	base.Image = "ghcr.io/actions/actions-runner:latest"
	if _, err := New(newFakeEngine(), base); err == nil {
		t.Fatal("mutable image accepted")
	}
	base = testOptions(nil)
	if _, err := New(newFakeEngine(), base); err == nil {
		t.Fatal("missing artifact sink accepted")
	}
}

func TestFileArtifactSinkUsesPrivateAtomicFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	jobs := newTestJobStore(t, filepath.Join(root, "state"))
	sink, err := NewFileArtifactSink(filepath.Join(root, "logs"), filepath.Join(root, "diag"), jobs, testJobACL{}, ArtifactPolicy{
		MaxFileSizeBytes: 1 << 20, RawDiagnosticMaxInputBytes: 2 << 20,
		Retention: time.Hour, TotalCapBytes: 4 << 20, CleanupEvery: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := ArtifactMetadata{ContainerID: "abcdef0123456789", PoolID: "org", WorkerName: "worker/unsafe", StartedAt: time.Unix(1, 0)}
	log, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = log.Write([]byte("runner output"))
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	record, err := jobs.FindByRunner(context.Background(), metadata.PoolID, metadata.WorkerName)
	if err != nil {
		t.Fatal(err)
	}
	firstContents, err := os.ReadFile(record.LogPath)
	if err != nil || string(firstContents) != "runner output" {
		t.Fatalf("first log = %q err=%v", firstContents, err)
	}
	retry, err := sink.OpenLog(context.Background(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = retry.Write([]byte("replacement output"))
	beforePublish, err := os.ReadFile(record.LogPath)
	if err != nil || string(beforePublish) != "runner output" {
		t.Fatalf("retry modified durable log before close: %q err=%v", beforePublish, err)
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
	afterPublish, err := os.ReadFile(record.LogPath)
	if err != nil || string(afterPublish) != "replacement output" {
		t.Fatalf("atomic replacement log = %q err=%v", afterPublish, err)
	}
	if err := sink.WriteDiagnostics(context.Background(), metadata, strings.NewReader("tar bytes")); err != nil {
		t.Fatal(err)
	}
	logs, _ := filepath.Glob(filepath.Join(root, "logs", "*.log"))
	diagnostics, _ := filepath.Glob(filepath.Join(root, "diag", "*.tar.gz"))
	if len(logs) != 1 || len(diagnostics) != 1 || strings.Contains(filepath.Base(logs[0]), "/") {
		t.Fatalf("logs=%v diagnostics=%v", logs, diagnostics)
	}
	if info, err := os.Stat(logs[0]); err != nil || info.Size() != int64(len("replacement output")) {
		t.Fatalf("log info=%v error=%v", info, err)
	}
}

func newTestRuntime(t *testing.T, engine *fakeEngine, sink ArtifactSink) *Runtime {
	t.Helper()
	runtime, err := New(engine, testOptions(sink))
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func testOptions(sink ArtifactSink) Options {
	return Options{
		HostID: "melo-desk-001", ControllerVersion: "test", Image: pinnedTestImage,
		DockerLogMaxSizeBytes: 10 << 20, DockerLogMaxFiles: 3,
		IdleConfirmationWindow: time.Millisecond, FinalizationTimeout: time.Second,
		Artifacts: sink,
	}
}

type fakeContainer struct {
	id          string
	labels      map[string]string
	state       string
	hookState   string
	created     int64
	stateReads  int
	changeAfter int
	changeTo    string
	waitResult  chan containertypes.WaitResponse
	waitError   chan error
}

type fakeEngine struct {
	mu                  sync.Mutex
	imagePresent        bool
	pulls               int
	pullImage           string
	created             client.ContainerCreateOptions
	containers          map[string]*fakeContainer
	calls               []string
	stops               int
	kills               int
	stopTimeout         *int
	attached            bytes.Buffer
	attachWrites        int
	attachWriteErrorAt  int
	attachCloseWriteErr error
	platform            system.Info
	blockLogs           bool
	activeLogs          int
	maximumLogs         int
	resourceCopyErr     error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		imagePresent: true,
		containers:   map[string]*fakeContainer{},
		platform:     system.Info{OSType: "linux", Architecture: "amd64"},
	}
}

func (e *fakeEngine) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.imagePresent {
		return client.ImageInspectResult{}, cerrdefs.ErrNotFound.WithMessage("missing image")
	}
	return client.ImageInspectResult{}, nil
}

func (e *fakeEngine) ImagePull(_ context.Context, image string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	e.mu.Lock()
	e.pulls++
	e.pullImage = image
	e.imagePresent = true
	e.mu.Unlock()
	return &fakePull{}, nil
}

func (e *fakeEngine) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return client.SystemInfoResult{Info: e.platform}, nil
}

func (e *fakeEngine) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	items := make([]containertypes.Summary, 0, len(e.containers))
	for _, container := range e.containers {
		items = append(items, containertypes.Summary{ID: container.id, Labels: cloneLabels(container.labels), State: containertypes.ContainerState(container.state), Created: container.created})
	}
	return client.ContainerListResult{Items: items}, nil
}

func (e *fakeEngine) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.created = options
	id := "created-container"
	e.containers[id] = &fakeContainer{
		id: id, labels: cloneLabels(options.Config.Labels), state: "created", hookState: "idle", created: time.Now().Unix(),
		waitResult: make(chan containertypes.WaitResponse, 1), waitError: make(chan error, 1),
	}
	return client.ContainerCreateResult{ID: id}, nil
}

func (e *fakeEngine) ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	connection := &recordingConnection{engine: e}
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(connection, "")}, nil
}

func (e *fakeEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	container, ok := e.containers[id]
	if !ok {
		return client.ContainerInspectResult{}, cerrdefs.ErrNotFound.WithMessage("container missing")
	}
	return client.ContainerInspectResult{Container: containertypes.InspectResponse{
		ID:     id,
		State:  &containertypes.State{Running: container.state == "running"},
		Config: &containertypes.Config{Labels: cloneLabels(container.labels)},
	}}, nil
}

func (e *fakeEngine) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[id].state = "running"
	e.calls = append(e.calls, "start:"+id)
	return client.ContainerStartResult{}, nil
}

func (e *fakeEngine) ContainerWait(_ context.Context, id string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	container := e.containers[id]
	return client.ContainerWaitResult{Result: container.waitResult, Error: container.waitError}
}

func (e *fakeEngine) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	e.mu.Lock()
	if e.blockLogs {
		e.activeLogs++
		if e.activeLogs > e.maximumLogs {
			e.maximumLogs = e.activeLogs
		}
		e.mu.Unlock()
		return &blockingLogReader{closed: make(chan struct{}), onClose: func() {
			e.mu.Lock()
			e.activeLogs--
			e.mu.Unlock()
		}}, nil
	}
	e.mu.Unlock()
	var buffer bytes.Buffer
	payload := []byte("runner output\n")
	header := make([]byte, 8)
	header[0] = 1 // Docker multiplexed stdout stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = buffer.Write(header)
	_, _ = buffer.Write(payload)
	return io.NopCloser(bytes.NewReader(buffer.Bytes())), nil
}

func (e *fakeEngine) CopyFromContainer(_ context.Context, id string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	container, ok := e.containers[id]
	if !ok {
		return client.CopyFromContainerResult{}, cerrdefs.ErrNotFound.WithMessage("container missing")
	}
	e.calls = append(e.calls, "copy:"+options.SourcePath)
	if options.SourcePath == defaultStatePath {
		container.stateReads++
		state := container.hookState
		if container.changeAfter > 0 && container.stateReads > container.changeAfter {
			state = container.changeTo
			container.hookState = state
		}
		return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(tarFile("state", []byte(state))))}, nil
	}
	if options.SourcePath == defaultResourceEvidencePath {
		if e.resourceCopyErr != nil {
			return client.CopyFromContainerResult{}, e.resourceCopyErr
		}
		return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(tarFile("cgroup-terminal.json", []byte(completeResourceEvidence))))}, nil
	}
	return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(tarFile("_diag/Runner_test.log", []byte("diagnostics"))))}, nil
}

func (e *fakeEngine) ContainerStop(_ context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error) {
	e.mu.Lock()
	container, ok := e.containers[id]
	if !ok {
		e.mu.Unlock()
		return client.ContainerStopResult{}, cerrdefs.ErrNotFound.WithMessage("container missing")
	}
	e.stops++
	if options.Timeout != nil {
		value := *options.Timeout
		e.stopTimeout = &value
	}
	container.state = "exited"
	e.calls = append(e.calls, "stop:"+id)
	e.mu.Unlock()
	container.waitResult <- containertypes.WaitResponse{StatusCode: 0}
	return client.ContainerStopResult{}, nil
}

func (e *fakeEngine) ContainerKill(_ context.Context, id string, _ client.ContainerKillOptions) (client.ContainerKillResult, error) {
	e.mu.Lock()
	container, ok := e.containers[id]
	if !ok {
		e.mu.Unlock()
		return client.ContainerKillResult{}, cerrdefs.ErrNotFound.WithMessage("container missing")
	}
	e.kills++
	container.state = "exited"
	e.calls = append(e.calls, "kill:"+id)
	e.mu.Unlock()
	container.waitResult <- containertypes.WaitResponse{StatusCode: 137}
	return client.ContainerKillResult{}, nil
}

func (e *fakeEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, "remove:"+id)
	delete(e.containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (e *fakeEngine) addContainer(id, hookState, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[id] = &fakeContainer{
		id: id, state: state, hookState: hookState, created: time.Now().Unix(),
		labels:     map[string]string{managedLabel: "true", hostLabel: "melo-desk-001", poolLabel: "org", workerNameLabel: id, startedAtLabel: time.Now().UTC().Format(time.RFC3339Nano)},
		waitResult: make(chan containertypes.WaitResponse, 1), waitError: make(chan error, 1),
	}
}

func (e *fakeEngine) changeStateAfterReads(id string, reads int, state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.containers[id].changeAfter = reads
	e.containers[id].changeTo = state
}

func (e *fakeEngine) signalExit(id string) {
	e.mu.Lock()
	result := e.containers[id].waitResult
	e.mu.Unlock()
	result <- containertypes.WaitResponse{StatusCode: 0}
}

func (e *fakeEngine) hasContainer(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.containers[id]
	return ok
}

func (e *fakeEngine) setPlatform(osType, architecture string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.platform.OSType = osType
	e.platform.Architecture = architecture
}

func (e *fakeEngine) createOptions() client.ContainerCreateOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.created
}
func (e *fakeEngine) pullCount() int      { e.mu.Lock(); defer e.mu.Unlock(); return e.pulls }
func (e *fakeEngine) pulledImage() string { e.mu.Lock(); defer e.mu.Unlock(); return e.pullImage }
func (e *fakeEngine) stopCount() int      { e.mu.Lock(); defer e.mu.Unlock(); return e.stops }
func (e *fakeEngine) killCount() int      { e.mu.Lock(); defer e.mu.Unlock(); return e.kills }
func (e *fakeEngine) lastStopTimeout() *int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopTimeout == nil {
		return nil
	}
	value := *e.stopTimeout
	return &value
}
func (e *fakeEngine) callsSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}
func (e *fakeEngine) attachedInput() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attached.String()
}
func (e *fakeEngine) logStreamCounts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeLogs, e.maximumLogs
}

type recordingConnection struct {
	engine *fakeEngine
}

type blockingLogReader struct {
	closed  chan struct{}
	onClose func()
	once    sync.Once
}

func (r *blockingLogReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.EOF
}
func (r *blockingLogReader) Close() error {
	r.once.Do(func() {
		close(r.closed)
		if r.onClose != nil {
			r.onClose()
		}
	})
	return nil
}

func (*recordingConnection) Read([]byte) (int, error) { return 0, io.EOF }
func (c *recordingConnection) Write(value []byte) (int, error) {
	c.engine.mu.Lock()
	defer c.engine.mu.Unlock()
	c.engine.attachWrites++
	written, err := c.engine.attached.Write(value)
	if c.engine.attachWriteErrorAt == c.engine.attachWrites {
		return written, errors.New("write response lost after bytes were delivered")
	}
	return written, err
}
func (c *recordingConnection) CloseWrite() error {
	c.engine.mu.Lock()
	defer c.engine.mu.Unlock()
	return c.engine.attachCloseWriteErr
}
func (*recordingConnection) Close() error                     { return nil }
func (*recordingConnection) LocalAddr() net.Addr              { return testAddress("local") }
func (*recordingConnection) RemoteAddr() net.Addr             { return testAddress("remote") }
func (*recordingConnection) SetDeadline(time.Time) error      { return nil }
func (*recordingConnection) SetReadDeadline(time.Time) error  { return nil }
func (*recordingConnection) SetWriteDeadline(time.Time) error { return nil }

type testAddress string

func (a testAddress) Network() string { return string(a) }
func (a testAddress) String() string  { return string(a) }

func containsCall(calls []string, expected string) bool {
	for _, call := range calls {
		if call == expected {
			return true
		}
	}
	return false
}

type fakePull struct{}

func (*fakePull) Read([]byte) (int, error)   { return 0, io.EOF }
func (*fakePull) Close() error               { return nil }
func (*fakePull) Wait(context.Context) error { return nil }
func (*fakePull) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}

type memoryArtifacts struct {
	mu          sync.Mutex
	logs        []*bufferWriteCloser
	diagnostics [][]byte
	adopted     [][]ArtifactMetadata
	events      []string
	finalizeErr error
	resources   []ResourceEvidence
}

type recordedWorkerStart struct {
	poolID  string
	outcome telemetry.WorkerStartOutcome
}

type recordedWorkerFinalization struct {
	poolID string
	value  telemetry.WorkerFinalization
}

type recordingTelemetry struct {
	mu            sync.Mutex
	starts        []recordedWorkerStart
	finalizations []recordedWorkerFinalization
}

func (r *recordingTelemetry) BeginReconcile(ctx context.Context) (context.Context, func(telemetry.ReconcileSnapshot, error)) {
	return ctx, func(telemetry.ReconcileSnapshot, error) {}
}
func (*recordingTelemetry) WorkerRegistered(context.Context, string, string, time.Duration, telemetry.WorkerStartOutcome) {
}
func (r *recordingTelemetry) WorkerStarted(_ context.Context, poolID, _ string, _ time.Duration, outcome telemetry.WorkerStartOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, recordedWorkerStart{poolID: poolID, outcome: outcome})
}
func (r *recordingTelemetry) WorkerFinalized(_ context.Context, poolID string, value telemetry.WorkerFinalization) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizations = append(r.finalizations, recordedWorkerFinalization{poolID: poolID, value: value})
}
func (*recordingTelemetry) ObserveJobStarted(context.Context, string, time.Duration)  {}
func (*recordingTelemetry) ObserveJobCompleted(context.Context, string, string, bool) {}
func (r *recordingTelemetry) snapshot() ([]recordedWorkerStart, []recordedWorkerFinalization) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedWorkerStart(nil), r.starts...), append([]recordedWorkerFinalization(nil), r.finalizations...)
}

func (s *memoryArtifacts) OpenLog(context.Context, ArtifactMetadata) (io.WriteCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "open-log")
	writer := &bufferWriteCloser{}
	s.logs = append(s.logs, writer)
	return writer, nil
}
func (s *memoryArtifacts) WriteDiagnostics(_ context.Context, _ ArtifactMetadata, source io.Reader) error {
	content, err := io.ReadAll(source)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "diagnostics")
	s.diagnostics = append(s.diagnostics, content)
	return err
}
func (s *memoryArtifacts) WriteResourceEvidence(_ context.Context, _ ArtifactMetadata, evidence ResourceEvidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "resources")
	s.resources = append(s.resources, evidence)
	return nil
}
func (s *memoryArtifacts) Finalize(context.Context, ArtifactMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "finalize")
	return s.finalizeErr
}
func (s *memoryArtifacts) AdoptAndCleanup(_ context.Context, metadata []ArtifactMetadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "adopt")
	s.adopted = append(s.adopted, append([]ArtifactMetadata(nil), metadata...))
	return nil
}

func (r *Runtime) watchForTest(id string) *containerWatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.watches[id]
}

type testJobLocker struct{ mu sync.Mutex }

func (l *testJobLocker) Lock(context.Context) (func() error, error) {
	l.mu.Lock()
	return func() error { l.mu.Unlock(); return nil }, nil
}

type testJobACL struct{}

func (testJobACL) Harden(string) error { return nil }
func (testJobACL) Verify(string) error { return nil }

func newTestJobStore(t *testing.T, directory string) jobindex.Store {
	t.Helper()
	store, err := jobindex.NewFileStore(directory, &testJobLocker{}, testJobACL{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type bufferWriteCloser struct{ bytes.Buffer }

func (*bufferWriteCloser) Close() error { return nil }

func tarFile(name string, content []byte) []byte {
	var buffer bytes.Buffer
	w := tar.NewWriter(&buffer)
	_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))})
	_, _ = w.Write(content)
	_ = w.Close()
	return buffer.Bytes()
}

func cloneLabels(labels map[string]string) map[string]string {
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

func assertCallBefore(t *testing.T, calls []string, first, second string) {
	t.Helper()
	firstIndex, secondIndex := -1, -1
	for index, call := range calls {
		if call == first && firstIndex < 0 {
			firstIndex = index
		}
		if call == second && secondIndex < 0 {
			secondIndex = index
		}
	}
	if firstIndex < 0 || secondIndex < 0 || firstIndex > secondIndex {
		t.Fatalf("calls = %v; want %q before %q", calls, first, second)
	}
}

var _ Engine = (*fakeEngine)(nil)
var _ client.ImagePullResponse = (*fakePull)(nil)
var _ = errors.Is
