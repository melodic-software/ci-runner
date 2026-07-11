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
	statuses      []control.Status
	statusCalls   int
	shutdownCalls int
}

func (f *fakeControllerControl) Status(context.Context) (control.Status, error) {
	index := f.statusCalls
	f.statusCalls++
	if index >= len(f.statuses) {
		index = len(f.statuses) - 1
	}
	return f.statuses[index], nil
}

func (f *fakeControllerControl) Shutdown(_ context.Context, _ string, expected control.Status, _ bool) (control.Status, error) {
	f.shutdownCalls++
	status := expected
	status.ShuttingDown = true
	return status, nil
}

type fakeProcessObserver struct{ handle *fakeProcessHandle }

func (f fakeProcessObserver) Open(uint32) (host.ProcessHandle, error) { return f.handle, nil }

type fakeProcessHandle struct{ exitCode uint32 }

func (f *fakeProcessHandle) Wait(context.Context) (uint32, error) { return f.exitCode, nil }
func (*fakeProcessHandle) Close() error                           { return nil }

type fakeTaskStarter struct{ names []string }

func (f *fakeTaskStarter) Start(_ context.Context, name string) error {
	f.names = append(f.names, name)
	return nil
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
		Store:        store,
		Gaming:       fakeGamingHost{},
		ForceStop:    force,
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

func TestControllerRestartUsesCleanHandshakeAndScheduledTask(t *testing.T) {
	store := state.NewMemoryStore()
	now := time.Now().UTC()
	_ = store.SaveObserved(context.Background(), model.ObservedState{SchemaVersion: 1, Phase: model.PhaseDisabled, HeartbeatAt: now})
	application, _, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{statuses: []control.Status{
		{ProcessID: 100, Version: "old", Phase: model.PhaseDisabled},
		{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
	}}
	tasks := &fakeTaskStarter{}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 1}}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if controlClient.shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", controlClient.shutdownCalls)
	}
	if len(tasks.names) != 0 {
		t.Fatalf("running restart must rely on Task Scheduler recovery, direct starts = %#v", tasks.names)
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
	application, _, _ := newTestApplication(t, "", store, nil)
	controlClient := &fakeControllerControl{statuses: []control.Status{
		{ProcessID: 100, AssignedJobCount: 1, ActiveJobCount: 1, ActiveWorkerCount: 1},
		{ProcessID: 200, Version: buildinfo.Version, Phase: model.PhaseStarting},
	}}
	application.dependencies.Control = controlClient
	application.dependencies.Processes = fakeProcessObserver{handle: &fakeProcessHandle{exitCode: 1}}
	tasks := &fakeTaskStarter{}
	application.dependencies.Tasks = tasks
	application.dependencies.Config.Controller.ShutdownPollInterval.Duration = time.Millisecond
	if code := application.Run(context.Background(), []string{"host", "controller", "restart"}); code != ExitOK {
		t.Fatalf("exit code %d, want success", code)
	}
	if controlClient.shutdownCalls != 1 || len(tasks.names) != 0 {
		t.Fatalf("shutdown=%d task starts=%#v", controlClient.shutdownCalls, tasks.names)
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
