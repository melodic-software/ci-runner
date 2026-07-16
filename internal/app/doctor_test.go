package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

type doctorControlFake struct {
	status control.Status
	err    error
}

func (f doctorControlFake) Status(context.Context) (control.Status, error) { return f.status, f.err }
func (doctorControlFake) Shutdown(context.Context, string, control.Status, bool) (control.Status, error) {
	return control.Status{}, errors.New("not implemented")
}

type doctorInspectorFake struct {
	beforeInspect func()
	request       DoctorInspection
	checks        []DoctorCheck
}

func (f *doctorInspectorFake) Inspect(_ context.Context, request DoctorInspection) []DoctorCheck {
	if f.beforeInspect != nil {
		f.beforeInspect()
	}
	f.request = request
	return append([]DoctorCheck(nil), f.checks...)
}

func TestDoctorDisabledHealthyRequiresLiveControllerAndExitsZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseDisabled))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseDisabled, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusStopped}}
	inspector := &doctorInspectorFake{checks: []DoctorCheck{
		{Name: "environment", Healthy: true, Detail: "verified"},
		{Name: "bitlocker", Skipped: true, Detail: "explicit opt-in required"},
		{Name: "github-jit-proof", Skipped: true, Detail: "proven live on rollout only"},
	}}
	application.dependencies.Doctor = inspector

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitOK {
		t.Fatalf("doctor exit code = %d; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[PASS] controller-control-plane") ||
		!strings.Contains(out.String(), "[SKIP] bitlocker") ||
		!strings.Contains(out.String(), "[SKIP] github-jit-proof") ||
		inspector.request.RequireDocker || inspector.request.IncludeElevated {
		t.Fatalf("unexpected doctor result or Docker requirement: %#v\n%s", inspector.request, out.String())
	}
}

func TestDoctorJSONDefaultsToNonElevatedInspectionWithoutWarning(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseDisabled))
	application, out, errOut := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseDisabled, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusStopped}}
	inspector := &doctorInspectorFake{checks: []DoctorCheck{{
		Name: "bitlocker", Skipped: true, Detail: "explicit opt-in required",
	}}}
	application.dependencies.Doctor = inspector

	if code := application.Run(context.Background(), []string{"host", "doctor", "--json"}); code != ExitOK {
		t.Fatalf("doctor exit code = %d; stdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if inspector.request.IncludeElevated || errOut.Len() != 0 {
		t.Fatalf("default JSON doctor requested elevation or warned: request=%#v stderr=%q", inspector.request, errOut.String())
	}
	var result struct {
		Checks []DoctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, out.String())
	}
	if check := doctorCheckNamed(t, result.Checks, "bitlocker"); !check.Skipped {
		t.Fatalf("default JSON BitLocker check = %#v, want skipped", check)
	}
}

func TestDoctorIncludeElevatedWarnsBeforeInspectionAndPreservesJSON(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseDisabled))
	application, out, errOut := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseDisabled, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusStopped}}
	warnedBeforeInspection := false
	inspector := &doctorInspectorFake{
		beforeInspect: func() {
			warnedBeforeInspection = strings.Contains(errOut.String(), "may open an Administrator UAC prompt")
		},
		checks: []DoctorCheck{{Name: "bitlocker", Healthy: true, Detail: "verified"}},
	}
	application.dependencies.Doctor = inspector

	if code := application.Run(context.Background(), []string{"host", "doctor", "--json", "--include-elevated"}); code != ExitOK {
		t.Fatalf("doctor exit code = %d; stdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !inspector.request.IncludeElevated || !warnedBeforeInspection {
		t.Fatalf("elevated request or warning order is wrong: request=%#v warnedBeforeInspection=%t stderr=%q", inspector.request, warnedBeforeInspection, errOut.String())
	}
	var result struct {
		Checks []DoctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("doctor --json output was corrupted by the warning: %v\n%s", err, out.String())
	}
}

func TestDoctorRejectsPersistedReadyStateWhenControllerIsUnavailable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseReady))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{err: control.ErrUnavailable}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d", code, ExitDegraded)
	}
	if !strings.Contains(out.String(), "[FAIL] controller-control-plane") {
		t.Fatalf("doctor did not expose unavailable live controller:\n%s", out.String())
	}
}

func TestDoctorRejectsStaleObservedHeartbeatDespiteLiveControlPlane(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now.Add(-time.Hour), model.PhaseReady))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseReady, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d", code, ExitDegraded)
	}
	if !strings.Contains(out.String(), "[FAIL] observed-state") {
		t.Fatalf("doctor did not expose stale observed state:\n%s", out.String())
	}
}

func TestDoctorRequiresLocalEngineOnlyForEnabledComputePhase(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseReady))
	application, _, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseReady, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning}}
	inspector := &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}
	application.dependencies.Doctor = inspector

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitOK {
		t.Fatalf("doctor exit code = %d", code)
	}
	if !inspector.request.RequireDocker {
		t.Fatal("enabled ready phase did not require the fixed local Docker Engine")
	}
}

func TestObservedFreshnessLimitUsesConfiguredRequestsRetriesAndTargets(t *testing.T) {
	t.Parallel()
	got := observedFreshnessLimit(doctorTestConfig())
	want := 6*(70*time.Second+time.Minute) + 2*5*time.Second
	if got != want {
		t.Fatalf("observed freshness limit = %s, want %s", got, want)
	}
}

func TestDoctorAllowsOnlyBoundedListenerAcknowledgementTransition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		transition time.Time
		wantCode   int
		wantMarker string
	}{
		{name: "within-grace", transition: now.Add(-15 * time.Second), wantCode: ExitOK, wantMarker: "[PASS] github-listener/organization"},
		{name: "past-grace", transition: now.Add(-81 * time.Second), wantCode: ExitDegraded, wantMarker: "[FAIL] github-listener/organization"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := state.NewMemoryStore()
			_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
			observed := healthyDoctorObserved(now, model.PhaseReady)
			observed.Pools[0].CapacityAcknowledged = false
			observed.Pools[0].UpdatedAt = test.transition
			_ = store.SaveObserved(context.Background(), observed)
			application, out, _ := newTestApplication(t, "", store, nil)
			application.dependencies.Config = doctorTestConfig()
			application.dependencies.Now = func() time.Time { return now }
			application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseReady, Version: "1.2.3"}}
			application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
			application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

			if code := application.Run(context.Background(), []string{"host", "doctor"}); code != test.wantCode {
				t.Fatalf("doctor exit code = %d, want %d\n%s", code, test.wantCode, out.String())
			}
			if !strings.Contains(out.String(), test.wantMarker) {
				t.Fatalf("doctor output missing %q:\n%s", test.wantMarker, out.String())
			}
		})
	}
}

func doctorTestConfig() config.Config {
	return config.Config{
		Controller: config.Controller{
			ReconcileInterval: config.Duration{Duration: 5 * time.Second},
			LocalProbeTimeout: config.Duration{Duration: time.Second},
			StartupTimeout:    config.Duration{Duration: time.Second},
		},
		GitHub: config.GitHub{
			RequestTimeout: config.Duration{Duration: 70 * time.Second},
			Retry: config.Retry{
				Maximum:     config.Duration{Duration: time.Minute},
				MaxAttempts: 6,
			},
			Targets: []config.Target{{ID: "organization"}},
		},
	}
}

func healthyDoctorObserved(now time.Time, phase model.Phase) model.ObservedState {
	return model.ObservedState{
		SchemaVersion: 1, Phase: phase, Version: "1.2.3", HeartbeatAt: now,
		Pools: []model.PoolObservation{{
			ID: "organization", ScaleSetID: 42, ListenerID: "listener", CapacityAcknowledged: true, UpdatedAt: now,
		}},
	}
}
