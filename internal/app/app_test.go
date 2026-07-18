package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/secret"
	"github.com/melodic-software/ci-runner/internal/state"
)

type fakeGamingHost struct{ inventory host.GamingInventory }

func (f fakeGamingHost) Inventory(context.Context) host.GamingInventory { return f.inventory }
func (fakeGamingHost) StopAll(context.Context) error {
	return errors.New("CLI must not stop host directly")
}
func (fakeGamingHost) Verify(context.Context) (host.GamingVerification, error) {
	return host.GamingVerification{}, errors.New("CLI must not verify controller work")
}

type fakeForceStopper struct {
	preview  []controller.ForceStopTarget
	executed bool
	err      error
}

type fakeSecretImporter struct {
	source      string
	destination string
	err         error
}

type fakeControllerControl struct {
	statuses             []control.Status
	statusErrors         []error
	statusErr            error
	statusCalls          int
	shutdownCalls        int
	omitRestartRequestID bool
}

const testRestartRequestID = "test-restart-request-1"

func (f *fakeControllerControl) Status(context.Context) (control.Status, error) {
	index := f.statusCalls
	f.statusCalls++
	var status control.Status
	if len(f.statuses) > 0 {
		statusIndex := min(index, len(f.statuses)-1)
		status = f.statuses[statusIndex]
	}
	if index < len(f.statusErrors) && f.statusErrors[index] != nil {
		return status, f.statusErrors[index]
	}
	if index >= len(f.statusErrors) && f.statusErr != nil {
		return status, f.statusErr
	}
	return status, nil
}

func (f *fakeControllerControl) Shutdown(_ context.Context, _ string, expected control.Status, restart bool) (control.Status, error) {
	f.shutdownCalls++
	status := expected
	status.ShuttingDown = true
	if restart && !f.omitRestartRequestID {
		status.RestartRequestID = testRestartRequestID
	}
	return status, nil
}

type fakeRestartReceiptReader struct {
	receipt model.RestartReceipt
	err     error
	loads   int
}

func (f *fakeRestartReceiptReader) LoadRestartReceipt(context.Context) (model.RestartReceipt, error) {
	f.loads++
	return f.receipt, f.err
}

type fakeProcessObserver struct{ handle *fakeProcessHandle }

func (f fakeProcessObserver) Open(uint32) (host.ProcessHandle, error) { return f.handle, nil }

type fakeProcessHandle struct {
	exitCode uint32
	waited   bool
}

func (f *fakeProcessHandle) Wait(context.Context) (uint32, error) {
	f.waited = true
	return f.exitCode, nil
}
func (*fakeProcessHandle) Close() error { return nil }

type fakeTaskStarter struct {
	names   []string
	errors  []error
	err     error
	onStart func()
}

func (f *fakeTaskStarter) Start(_ context.Context, name string) error {
	if f.onStart != nil {
		f.onStart()
	}
	index := len(f.names)
	f.names = append(f.names, name)
	if index < len(f.errors) {
		return f.errors[index]
	}
	return f.err
}

func (f *fakeSecretImporter) Import(_ context.Context, source, destination string) (secret.ImportResult, error) {
	f.source = source
	f.destination = destination
	if f.err != nil {
		return secret.ImportResult{}, f.err
	}
	return secret.ImportResult{Path: destination, Fingerprint: "abc123", ImportedAt: time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC)}, nil
}

type cancelAfterDesiredStore struct {
	state.Store
	cancel context.CancelFunc
}

func (s cancelAfterDesiredStore) SaveDesired(ctx context.Context, desired model.DesiredState) error {
	if err := s.Store.SaveDesired(ctx, desired); err != nil {
		return err
	}
	s.cancel()
	return nil
}

func (f *fakeForceStopper) Preview(context.Context) ([]controller.ForceStopTarget, error) {
	return append([]controller.ForceStopTarget(nil), f.preview...), nil
}

func (f *fakeForceStopper) Execute(_ context.Context, preview []controller.ForceStopTarget) ([]controller.ForceStopTarget, error) {
	f.executed = true
	return preview, f.err
}

func newTestApplication(t *testing.T, input string, store state.Store, force ForceStopper) (*Application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	application, err := New(Dependencies{
		Config: config.Config{Controller: config.Controller{
			LocalProbeTimeout: config.Duration{Duration: time.Second}, StartupTimeout: config.Duration{Duration: time.Second},
		}},
		Store:     store,
		Gaming:    fakeGamingHost{},
		ForceStop: force,
		RestartReceipts: &fakeRestartReceiptReader{receipt: model.RestartReceipt{
			SchemaVersion: 1, RequestID: testRestartRequestID, ProcessID: 100,
			Version: buildinfo.Version, CompletedAt: time.Date(2026, 7, 9, 1, 2, 4, 0, time.UTC),
		}},
		Now:          func() time.Time { return time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC) },
		PollInterval: time.Millisecond,
	}, strings.NewReader(input), &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	return application, &out, &errOut
}

func TestStatusDisabledAndHealthyExitsZero(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDisabled, HeartbeatAt: now})
	application, out, _ := newTestApplication(t, "", store, nil)
	if code := application.Run(context.Background(), []string{"host", "status"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(out.String(), "Phase: disabled") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestStatusJSONExposesStableBusyWorkContract(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
		Workers: []model.Worker{
			{ID: "busy", PoolID: "org", Name: "worker-busy", State: model.WorkerBusy, JobID: "42", StartedAt: now},
			{ID: "idle", PoolID: "org", Name: "worker-idle", State: model.WorkerIdle, StartedAt: now},
		},
	})
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Control = &fakeControllerControl{statuses: []control.Status{
		{ProcessID: 4242, Version: "1.2.3", Phase: model.PhaseReady, ActiveJobCount: 1},
	}}
	if code := application.Run(context.Background(), []string{"host", "status", "--json"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	for _, expected := range []string{
		`"schemaVersion": 1`, `"controllerAvailable": true`, `"activeJobCount": 1`,
		`"workerId": "busy"`, `"pid": 4242`, `"version": "1.2.3"`,
	} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("status JSON missing %s: %s", expected, out.String())
		}
	}
}

func TestDisableDefaultsToWaitAndCtrlCDetaches(t *testing.T) {
	memory := state.NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := cancelAfterDesiredStore{Store: memory, cancel: cancel}
	application, out, _ := newTestApplication(t, "", store, nil)
	if code := application.Run(ctx, []string{"host", "disable"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	desired, err := memory.LoadDesired(context.Background())
	if err != nil || desired.Mode != model.ModeDisabled {
		t.Fatalf("desired = %#v, err=%v", desired, err)
	}
	if !strings.Contains(out.String(), "Detached") {
		t.Fatalf("expected detach output: %s", out.String())
	}
}

func TestGamingModeInventoriesBeforeConfirmationAndOnlyWritesIntent(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
		Workers:       []model.Worker{{ID: "one", PoolID: "org", Name: "worker-one", State: model.WorkerBusy, JobID: "42"}},
	})
	application, out, _ := newTestApplication(t, "yes\n", store, nil)
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{
		NonCIContainers:      []host.Container{{Name: "database", Image: "postgres"}},
		RunningDistributions: []string{"Ubuntu-24.04"},
	}}
	if code := application.Run(context.Background(), []string{"host", "game", "--detach"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	desired, _ := store.LoadDesired(context.Background())
	if desired.Mode != model.ModeGaming {
		t.Fatalf("desired mode = %q", desired.Mode)
	}
	for _, text := range []string{"job=42", "database", "Ubuntu-24.04"} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("output missing %q: %s", text, out.String())
		}
	}
}

func TestForceStopRequiresZeroCapacityAndExactTypedConfirmation(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseDraining,
		HeartbeatAt:   now,
		Pools:         []model.PoolObservation{{ID: "org", MaxCapacity: 0}},
	})
	force := &fakeForceStopper{preview: []controller.ForceStopTarget{{WorkerID: "one", Name: "worker-one", PoolID: "org", State: model.WorkerBusy, JobID: "42"}}}
	application, _, _ := newTestApplication(t, "FORCE STOP\n", store, force)
	if code := application.Run(context.Background(), []string{"host", "force-stop"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !force.executed {
		t.Fatal("expected force-stop execution")
	}
	desired, _ := store.LoadDesired(context.Background())
	if desired.Mode != model.ModeDisabled {
		t.Fatalf("desired mode = %q, want disabled", desired.Mode)
	}
}

func TestForceStopRejectsStalePreview(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDraining, HeartbeatAt: now})
	force := &fakeForceStopper{
		preview: []controller.ForceStopTarget{{WorkerID: "one", Name: "worker-one", PoolID: "org", State: model.WorkerBusy}},
		err:     controller.ErrForceStopStateChanged,
	}
	application, _, _ := newTestApplication(t, "FORCE STOP\n", store, force)
	if code := application.Run(context.Background(), []string{"host", "force-stop"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want %d", code, ExitStateChanged)
	}
}

func TestDegradedTimeoutHasDistinctExitCode(t *testing.T) {
	store := state.NewMemoryStore()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseDegraded,
		HeartbeatAt:   time.Now().UTC(),
		Problems:      []model.Problem{{Code: "docker-start-timeout", Message: "timed out"}},
	})
	application, _, _ := newTestApplication(t, "", store, nil)
	if code := application.Run(context.Background(), []string{"host", "status"}); code != ExitOperationTimedOut {
		t.Fatalf("exit code %d, want %d", code, ExitOperationTimedOut)
	}
}

func TestSecretImportMapsConfiguredSecretIDToExactDPAPIPath(t *testing.T) {
	store := state.NewMemoryStore()
	application, out, _ := newTestApplication(t, "", store, nil)
	importer := &fakeSecretImporter{}
	application.dependencies.Secrets = importer
	application.dependencies.Config.Paths.Secrets = `C:\Users\test\AppData\Local\ci-runner\secrets`
	application.dependencies.Config.GitHub.Targets = []config.Target{{SecretID: "organization-host"}}
	source := `C:\input.pem`
	if code := application.Run(context.Background(), []string{"secret", "import", "--file", source}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	want := `C:\Users\test\AppData\Local\ci-runner\secrets/organization-host.dpapi`
	if strings.ReplaceAll(importer.destination, `\`, "/") != strings.ReplaceAll(want, `\`, "/") {
		t.Fatalf("destination = %q, want %q", importer.destination, want)
	}
	if importer.source != source {
		t.Fatalf("source = %q, want %q", importer.source, source)
	}
	if !strings.Contains(out.String(), "Plaintext source PEM removed") || !strings.Contains(out.String(), "not media sanitization") || !strings.Contains(out.String(), source) || !strings.Contains(out.String(), "GitHub App fingerprint (Base64 SHA-256)") {
		t.Fatalf("success output does not disclose source-removal contract: %s", out.String())
	}
}

func TestSecretImportDoesNotReportSuccessWhenSourceRemovalFails(t *testing.T) {
	store := state.NewMemoryStore()
	application, out, errOut := newTestApplication(t, "", store, nil)
	removeErr := errors.New("remove plaintext source: access denied")
	application.dependencies.Secrets = &fakeSecretImporter{err: removeErr}
	application.dependencies.Config.Paths.Secrets = `C:\Users\test\AppData\Local\ci-runner\secrets`
	application.dependencies.Config.GitHub.Targets = []config.Target{{SecretID: "organization-host"}}

	if code := application.Run(context.Background(), []string{"secret", "import", "--file", `C:\input.pem`}); code != ExitCredential {
		t.Fatalf("exit code %d, want %d", code, ExitCredential)
	}
	if strings.Contains(out.String(), "Imported GitHub App key") || strings.Contains(out.String(), "Plaintext source PEM removed") {
		t.Fatalf("failed import reported success: %s", out.String())
	}
	if !strings.Contains(errOut.String(), removeErr.Error()) {
		t.Fatalf("failure output = %q, want %q", errOut.String(), removeErr.Error())
	}
}

func TestConfigValidateJSONIsReadOnlyAndStable(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := config.Config{
		SchemaVersion: 1,
		Host:          config.Host{ID: "melo-desk-001"},
		GitHub:        config.GitHub{Targets: make([]config.Target, 2)},
		Release: config.Release{
			CompatibilityManifest: `C:/Users/Test/AppData/Local/Programs/ci-runner/current/compatibility.json`,
		},
		Paths: config.Paths{
			Secrets:     `C:/Users/Test/AppData/Local/ci-runner/secrets`,
			State:       `C:/Users/Test/AppData/Local/ci-runner/state`,
			Logs:        `C:/Users/Test/AppData/Local/ci-runner/logs`,
			Diagnostics: `C:/Users/Test/AppData/Local/ci-runner/diagnostics`,
		},
	}
	if code := runConfigCommand(cfg, []string{"validate", "--json"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit code %d: %s", code, errOut.String())
	}
	var result configValidationResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode validation result: %v", err)
	}
	if !result.Valid || result.HostID != "melo-desk-001" || result.TargetCount != 2 {
		t.Fatalf("unexpected validation identity: %#v", result)
	}
	wantManifest := `c:\users\test\appdata\local\programs\ci-runner\current\compatibility.json`
	if result.Release.CompatibilityManifest != wantManifest {
		t.Fatalf("compatibilityManifest = %q, want %q", result.Release.CompatibilityManifest, wantManifest)
	}
	wantPaths := configValidationPaths{
		Secrets:     `c:\users\test\appdata\local\ci-runner\secrets`,
		State:       `c:\users\test\appdata\local\ci-runner\state`,
		Logs:        `c:\users\test\appdata\local\ci-runner\logs`,
		Diagnostics: `c:\users\test\appdata\local\ci-runner\diagnostics`,
	}
	if result.Paths != wantPaths {
		t.Fatalf("paths = %#v, want %#v", result.Paths, wantPaths)
	}
}

func TestControllerRestartExplicitlyStartsCanonicalTaskAfterCleanHandshake(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDisabled, HeartbeatAt: now})
	application, out, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version, Phase: model.PhaseDisabled},
			{},
			{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
		},
		statusErrors: []error{nil, control.ErrUnavailable, nil},
	}
	handle := &fakeProcessHandle{exitCode: ControllerRestartExitCode}
	receipts := application.dependencies.RestartReceipts.(*fakeRestartReceiptReader)
	tasks := &fakeTaskStarter{onStart: func() {
		if !handle.waited {
			t.Fatal("scheduled task was started before the old process exit was verified")
		}
		if receipts.loads != 1 {
			t.Fatal("scheduled task was started before the exact durable restart receipt was verified")
		}
	}}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: handle}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if controlClient.shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", controlClient.shutdownCalls)
	}
	if len(tasks.names) != 1 || tasks.names[0] != controllerTaskName {
		t.Fatalf("canonical task starts = %#v, want [%q]", tasks.names, controllerTaskName)
	}
	if !strings.Contains(out.String(), "exact durable completion receipt verified") ||
		!strings.Contains(out.String(), "Starting canonical scheduled task") || !strings.Contains(out.String(), "pid 200") {
		t.Fatalf("restart did not report explicit verified task recovery:\n%s", out.String())
	}
}

func TestControllerRestartDrainsBusyWorkWithoutForce(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{
		SchemaVersion: 1,
		Phase:         model.PhaseReady,
		HeartbeatAt:   now,
		Workers:       []model.Worker{{ID: "busy", PoolID: "org", State: model.WorkerBusy}},
	})
	application, out, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version, AssignedJobCount: 1, ActiveJobCount: 1, ActiveWorkerCount: 1},
			{},
			{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
		},
		statusErrors: []error{nil, control.ErrUnavailable, nil},
	}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	tasks := &fakeTaskStarter{}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d, want success", code)
	}
	if controlClient.shutdownCalls != 1 || len(tasks.names) != 1 || tasks.names[0] != controllerTaskName {
		t.Fatalf("shutdown=%d task starts=%#v", controlClient.shutdownCalls, tasks.names)
	}
	if !strings.Contains(out.String(), "assigned jobs: 1, active jobs: 1, active workers: 1") ||
		!strings.Contains(out.String(), "Existing work will finish naturally") {
		t.Fatalf("busy restart did not preserve the graceful-drain contract:\n%s", out.String())
	}
}

func TestControllerRestartRetriesCanonicalTaskAcrossIgnoreNewRace(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version, Phase: model.PhaseReady},
			{},
			{},
			{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
		},
		statusErrors: []error{nil, control.ErrUnavailable, control.ErrUnavailable, nil},
	}
	tasks := &fakeTaskStarter{errors: []error{nil, errors.New("task is already running")}}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d, want success", code)
	}
	if len(tasks.names) != 2 {
		t.Fatalf("canonical task starts = %#v, want two attempts across IgnoreNew race", tasks.names)
	}
	for _, name := range tasks.names {
		if name != controllerTaskName {
			t.Fatalf("started noncanonical task %q", name)
		}
	}
}

func TestControllerRestartFailsClosedWithoutVerifiedReplacement(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses:     []control.Status{{ProcessID: 100, Version: buildinfo.Version}, {}},
		statusErrors: []error{nil},
		statusErr:    control.ErrUnavailable,
	}
	tasks := &fakeTaskStarter{err: errors.New("scheduled task start failed")}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	application.dependencies.Config.Controller.StartupTimeout.Duration = 30 * time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOperationTimedOut {
		t.Fatalf("exit code %d, want timeout", code)
	}
	if len(tasks.names) != maxControllerTaskStartAttempts {
		t.Fatalf("canonical task starts = %d, want bounded maximum %d: %#v", len(tasks.names), maxControllerTaskStartAttempts, tasks.names)
	}
	if !strings.Contains(errOut.String(), "last canonical task start attempt failed") ||
		!strings.Contains(errOut.String(), "at most 4 canonical task start attempt") {
		t.Fatalf("timeout did not retain fail-closed evidence:\n%s", errOut.String())
	}
}

func TestControllerRestartRejectsReplacementAtWrongVersion(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version},
			{},
			{ProcessID: 200, Version: "unexpected"},
		},
		statusErrors: []error{nil, control.ErrUnavailable, nil},
	}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want state-changed", code)
	}
	if len(tasks.names) != 1 || !strings.Contains(errOut.String(), "does not match expected version") {
		t.Fatalf("wrong-version replacement was not rejected: tasks=%#v error=%s", tasks.names, errOut.String())
	}
}

func TestControllerRestartNeverTreatsUnavailableAsProofOfShutdown(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = &fakeControllerControl{statusErrors: []error{control.ErrUnavailable}}
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 0}}
	application.dependencies.Tasks = tasks
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitDegraded {
		t.Fatalf("exit code %d, want degraded", code)
	}
	if len(tasks.names) != 0 || !strings.Contains(errOut.String(), "cannot be proven") {
		t.Fatalf("unavailable control plane authorized task start: tasks=%#v error=%s", tasks.names, errOut.String())
	}
}

func TestControllerRestartRequiresDurableReceiptInsteadOfExitCode(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{statuses: []control.Status{{ProcessID: 100, Version: buildinfo.Version}}}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	application.dependencies.Tasks = tasks
	application.dependencies.RestartReceipts = &fakeRestartReceiptReader{err: state.ErrNotFound}
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want state-changed", code)
	}
	if len(tasks.names) != 0 || !strings.Contains(errOut.String(), "without a durable restart completion receipt") {
		t.Fatalf("dedicated exit code without receipt authorized task start: tasks=%#v error=%s", tasks.names, errOut.String())
	}
}

func TestControllerRestartAuthorizesWrongExitCodeWithVerifiedReceipt(t *testing.T) {
	store := state.NewMemoryStore()
	application, out, _ := newTestApplication(t, "", store, nil)
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version},
			{},
			{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
		},
		statusErrors: []error{nil, control.ErrUnavailable, nil},
	}
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 143}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d, want success", code)
	}
	if len(tasks.names) != 1 || !strings.Contains(out.String(), "durable completion receipt is verified") {
		t.Fatalf("verified receipt did not authorize restart under wrong exit code: tasks=%#v out=%s", tasks.names, out.String())
	}
}

func TestControllerRestartRejectsWrongExitCodeWithoutReceipt(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = &fakeControllerControl{statuses: []control.Status{{ProcessID: 100, Version: buildinfo.Version}}}
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 1}}
	application.dependencies.Tasks = tasks
	application.dependencies.RestartReceipts = &fakeRestartReceiptReader{err: state.ErrNotFound}
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want state-changed", code)
	}
	if len(tasks.names) != 0 || !strings.Contains(errOut.String(), "no matching completion receipt") {
		t.Fatalf("ordinary failure without receipt authorized restart: tasks=%#v error=%s", tasks.names, errOut.String())
	}
}

func TestControllerRestartRejectsWrongExitCodeWithMismatchedReceipt(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	receipts := application.dependencies.RestartReceipts.(*fakeRestartReceiptReader)
	receipts.receipt.RequestID = "other-request"
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = &fakeControllerControl{statuses: []control.Status{{ProcessID: 100, Version: buildinfo.Version}}}
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 1}}
	application.dependencies.Tasks = tasks
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want state-changed", code)
	}
	if len(tasks.names) != 0 || !strings.Contains(errOut.String(), "no matching completion receipt") {
		t.Fatalf("mismatched receipt authorized restart under wrong exit code: tasks=%#v error=%s", tasks.names, errOut.String())
	}
}

func TestControllerRestartRejectsMismatchedCompletionReceipt(t *testing.T) {
	tests := map[string]func(*model.RestartReceipt){
		"schema":     func(receipt *model.RestartReceipt) { receipt.SchemaVersion = 2 },
		"request ID": func(receipt *model.RestartReceipt) { receipt.RequestID = "other-request" },
		"process ID": func(receipt *model.RestartReceipt) { receipt.ProcessID = 999 },
		"version":    func(receipt *model.RestartReceipt) { receipt.Version = "unexpected" },
		"completion": func(receipt *model.RestartReceipt) { receipt.CompletedAt = time.Time{} },
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			store := state.NewMemoryStore()
			application, _, errOut := newTestApplication(t, "", store, nil)
			receipts := application.dependencies.RestartReceipts.(*fakeRestartReceiptReader)
			mutate(&receipts.receipt)
			tasks := &fakeTaskStarter{}
			application.dependencies.Control = &fakeControllerControl{statuses: []control.Status{{ProcessID: 100, Version: buildinfo.Version}}}
			application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
			application.dependencies.Tasks = tasks
			if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
				t.Fatalf("exit code %d, want state-changed", code)
			}
			if len(tasks.names) != 0 || !strings.Contains(errOut.String(), "does not match the authenticated request") {
				t.Fatalf("mismatched receipt authorized task start: tasks=%#v error=%s", tasks.names, errOut.String())
			}
		})
	}
}

func TestControllerRestartCanRejoinAuthenticatedDrain(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{
		statuses: []control.Status{
			{ProcessID: 100, Version: buildinfo.Version, ShuttingDown: true, RestartRequestID: testRestartRequestID},
			{},
			{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
		},
		statusErrors: []error{nil, control.ErrUnavailable, nil},
	}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: ControllerRestartExitCode}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d, want success", code)
	}
	if controlClient.shutdownCalls != 0 || len(tasks.names) != 1 {
		t.Fatalf("shutdown calls=%d task starts=%#v", controlClient.shutdownCalls, tasks.names)
	}
}

func TestControllerRestartRejectsAcknowledgementWithoutRequestID(t *testing.T) {
	store := state.NewMemoryStore()
	application, _, errOut := newTestApplication(t, "", store, nil)
	handle := &fakeProcessHandle{exitCode: ControllerRestartExitCode}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = &fakeControllerControl{
		statuses: []control.Status{{ProcessID: 100, Version: buildinfo.Version}}, omitRestartRequestID: true,
	}
	application.dependencies.Processes = fakeProcessObserver{handle: handle}
	application.dependencies.Tasks = tasks
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitStateChanged {
		t.Fatalf("exit code %d, want state-changed", code)
	}
	if handle.waited || len(tasks.names) != 0 || !strings.Contains(errOut.String(), "omitted its authenticated request ID") {
		t.Fatalf("missing request ID advanced restart: waited=%t tasks=%#v error=%s", handle.waited, tasks.names, errOut.String())
	}
}

func TestControllerStopForUpdateDrainsAndDoesNotRestartTask(t *testing.T) {
	store := state.NewMemoryStore()
	application, out, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{statuses: []control.Status{{
		ProcessID: 100, AssignedJobCount: 2, ActiveJobCount: 1, ActiveWorkerCount: 2,
	}}}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{}}
	application.dependencies.Tasks = tasks
	if code := application.Run(context.Background(), []string{"host", "controller", "stop-for-update"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if controlClient.shutdownCalls != 1 || len(tasks.names) != 0 {
		t.Fatalf("shutdown=%d task starts=%#v", controlClient.shutdownCalls, tasks.names)
	}
	if !strings.Contains(out.String(), "finish naturally") || !strings.Contains(out.String(), "stopped safely for update") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}
