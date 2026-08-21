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
	beforeInspect  func()
	request        DoctorInspection
	inspectContext context.Context
	checks         []DoctorCheck
}

func (f *doctorInspectorFake) Inspect(ctx context.Context, request DoctorInspection) []DoctorCheck {
	if f.beforeInspect != nil {
		f.beforeInspect()
	}
	f.request = request
	f.inspectContext = ctx
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

// TestDoctorLeavesTheInspectionRoomForTheElevatedProbeBudget asserts the
// criterion rather than the mechanism: a derived context expires no later than
// its parent, so budgeting the inspection as a whole -- on the machine-probe
// budget doctorTestConfig sets to 1s -- silently caps the elevated BitLocker
// probe far below the human budget it needs, and every per-check test still
// passes while the probe stays unpassable at human speed.
func TestDoctorLeavesTheInspectionRoomForTheElevatedProbeBudget(t *testing.T) {
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
	inspector := &doctorInspectorFake{checks: []DoctorCheck{{Name: "bitlocker", Healthy: true, Detail: "verified"}}}
	application.dependencies.Doctor = inspector

	if code := application.Run(context.Background(), []string{"host", "doctor", "--include-elevated"}); code != ExitOK {
		t.Fatalf("doctor exit code = %d; stdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if inspector.inspectContext == nil {
		t.Fatal("doctor never invoked the inspector")
	}
	deadline, bounded := inspector.inspectContext.Deadline()
	if bounded && time.Until(deadline) < defaultElevatedProbeTimeout {
		t.Fatalf("inspection context leaves %s, want room for the %s elevated probe budget", time.Until(deadline), defaultElevatedProbeTimeout)
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

func TestDoctorAdvisoryPendingRebootWarnsWithoutDegradingExitCode(t *testing.T) {
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
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{
		{Name: "pending-os-reboot", Advisory: true, Detail: "OS reboot pending (component-servicing)"},
	}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitOK {
		t.Fatalf("advisory pending reboot degraded doctor exit code to %d; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[WARN] pending-os-reboot: OS reboot pending (component-servicing)") {
		t.Fatalf("advisory pending reboot was not surfaced as WARN:\n%s", out.String())
	}
}

// verifyingGamingHost answers Verify, which fakeGamingHost deliberately refuses
// to do. Doctor is the one caller that legitimately verifies, so the gaming
// branch needs a host that can report a postcondition result.
type verifyingGamingHost struct {
	inventory    host.GamingInventory
	verification host.GamingVerification
	verifyErr    error
}

func (f verifyingGamingHost) Inventory(context.Context) host.GamingInventory { return f.inventory }
func (verifyingGamingHost) StopAll(context.Context) error {
	return errors.New("CLI must not stop host directly")
}

func (f verifyingGamingHost) Verify(context.Context) (host.GamingVerification, error) {
	return f.verification, f.verifyErr
}

func TestDoctorExitsZeroInGamingModeWhenTheDesktopProbeCouldNotAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeGaming, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseGaming))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseGaming, Version: "1.2.3"}}
	application.dependencies.Gaming = verifyingGamingHost{
		inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusUnknown, Problems: []string{"Docker Desktop status: probe did not complete"}},
		verification: host.GamingVerification{
			DesktopUnverified: true,
			DockerUnreachable: true,
			NoRunningWSL:      true,
		},
		verifyErr: errors.New("query Docker Desktop status: probe did not complete"),
	}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitOK {
		t.Fatalf("an unverifiable postcondition degraded the gaming-mode exit code to %d; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[WARN] gaming-postconditions") {
		t.Fatalf("an unverified postcondition must surface as WARN:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "desktopStopped=unverified") {
		t.Fatalf("an unchecked postcondition must not render as an observed failure:\n%s", out.String())
	}
}

func TestDoctorStillFailsInGamingModeWhenAPostconditionIsObservedViolated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeGaming, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseGaming))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseGaming, Version: "1.2.3"}}
	application.dependencies.Gaming = verifyingGamingHost{
		inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusUnknown},
		verification: host.GamingVerification{
			DesktopUnverified:    true,
			DockerUnreachable:    true,
			RunningDistributions: []string{"Ubuntu"},
		},
		verifyErr: errors.New("WSL distributions are still running: [Ubuntu]"),
	}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("an observed postcondition violation must degrade the exit code, got %d; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] gaming-postconditions") {
		t.Fatalf("an observed violation must surface as FAIL, not WARN:\n%s", out.String())
	}
}

func TestDoctorJSONPreservesAdvisoryClassification(t *testing.T) {
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
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{
		{Name: "pending-os-reboot", Advisory: true, Detail: "OS reboot pending (windows-update)"},
	}}

	if code := application.Run(context.Background(), []string{"host", "doctor", "--json"}); code != ExitOK {
		t.Fatalf("advisory pending reboot degraded JSON doctor exit code to %d; output:\n%s", code, out.String())
	}
	var result struct {
		Checks []DoctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, out.String())
	}
	check := doctorCheckNamed(t, result.Checks, "pending-os-reboot")
	if !check.Advisory || check.Healthy || check.Skipped {
		t.Fatalf("JSON pending reboot check = %#v, want an unhealthy advisory", check)
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

func TestListenerAcknowledgementGraceBudgetsAFullRetryEnvelopePerConvergenceLeg(t *testing.T) {
	t.Parallel()
	cfg := doctorTestConfig()
	got := listenerAcknowledgementGrace(cfg)
	want := listenerAcknowledgementConvergenceLegs*6*(70*time.Second+time.Minute) + 2*5*time.Second
	if got != want {
		t.Fatalf("listener acknowledgement grace = %s, want %s", got, want)
	}

	// Every term of the retry policy has to move the window, because a poll that
	// exhausts any of them is still legitimately retrying. Budgeting fewer
	// attempts than are configured is what let a benign busy-fleet poll trip a
	// hard fault.
	for _, test := range []struct {
		name   string
		mutate func(*config.Config)
	}{
		{name: "more-attempts", mutate: func(c *config.Config) { c.GitHub.Retry.MaxAttempts = 12 }},
		{name: "larger-backoff-maximum", mutate: func(c *config.Config) {
			c.GitHub.Retry.Maximum = config.Duration{Duration: 2 * time.Minute}
		}},
		// Jitter is applied after the backoff base is capped at Maximum, so a
		// policy-compliant wait can exceed bare Maximum.
		{name: "positive-jitter", mutate: func(c *config.Config) { c.GitHub.Retry.JitterRatio = 0.2 }},
		{name: "longer-request-timeout", mutate: func(c *config.Config) {
			c.GitHub.RequestTimeout = config.Duration{Duration: 2 * time.Minute}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wider := doctorTestConfig()
			test.mutate(&wider)
			if widened := listenerAcknowledgementGrace(wider); widened <= got {
				t.Fatalf("grace = %s, want more than the baseline %s", widened, got)
			}
		})
	}
}

// A wedged listener never acknowledges, so its lag grows without bound. Widening
// the window delays that verdict; it must not remove it.
func TestListenerAcknowledgementStaysAHardFaultForAWedgedListener(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	grace := listenerAcknowledgementGrace(doctorTestConfig())

	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	observed := healthyDoctorObserved(now, model.PhaseReady)
	observed.Pools[0].CapacityAcknowledged = false
	observed.Pools[0].UpdatedAt = now.Add(-10 * grace)
	_ = store.SaveObserved(context.Background(), observed)

	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseReady, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d (sustained non-acknowledgement must stay a hard fault)\n%s", code, ExitDegraded, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] github-listener/organization") {
		t.Fatalf("doctor output missing the listener failure:\n%s", out.String())
	}
}

func TestDoctorAllowsOnlyBoundedListenerAcknowledgementTransition(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	grace := listenerAcknowledgementGrace(doctorTestConfig())
	for _, test := range []struct {
		name       string
		transition time.Time
		wantCode   int
		wantMarker string
	}{
		{name: "within-grace", transition: now.Add(-15 * time.Second), wantCode: ExitOK, wantMarker: "[PASS] github-listener/organization"},
		// The one benign busy-fleet acknowledgement lag on record (ci-runner
		// alignment audit, D4). It exceeded the old window and degraded the exit
		// code; containing it is the whole point of the widened derivation, so it
		// is pinned as an absolute case rather than left implied by the boundary
		// below.
		{name: "benign-busy-fleet-lag", transition: now.Add(-129 * time.Second), wantCode: ExitOK, wantMarker: "[PASS] github-listener/organization"},
		{name: "at-grace", transition: now.Add(-grace), wantCode: ExitOK, wantMarker: "[PASS] github-listener/organization"},
		{name: "past-grace", transition: now.Add(-grace - time.Second), wantCode: ExitDegraded, wantMarker: "[FAIL] github-listener/organization"},
	} {
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

func TestDoctorFailsNeverReadyControllerWhenEnabledAndObservedDisabled(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseDisabled))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseDisabled, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d; output:\n%s", code, ExitDegraded, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] controller-reconcile-progress") {
		t.Fatalf("doctor did not expose never-ready reconcile progress:\n%s", out.String())
	}
}

func TestDoctorFailsGitHubListenerWhenAcknowledgedCapacityIsZeroWithDesiredWorkers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	observed := healthyDoctorObserved(now, model.PhaseReady)
	observed.Pools[0].CapacityAcknowledged = true
	observed.Pools[0].DesiredWorkers = 3
	observed.Pools[0].MaxCapacity = 0
	_ = store.SaveObserved(context.Background(), observed)
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseReady, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d; output:\n%s", code, ExitDegraded, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] github-listener/organization") {
		t.Fatalf("doctor did not fail the acknowledged-but-starved listener:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "desiredWorkers=3") || !strings.Contains(out.String(), "capacity=0") {
		t.Fatalf("listener detail missing desired/planned vs acknowledged capacity:\n%s", out.String())
	}
}

func TestDoctorFailsCapacityStarvedWhenResourceConstrainedAndAllPoolsZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	store := state.NewMemoryStore()
	_ = store.SaveDesired(context.Background(), model.DesiredState{SchemaVersion: 1, Mode: model.ModeEnabled, UpdatedAt: now})
	_ = store.SaveObserved(context.Background(), healthyDoctorObserved(now, model.PhaseResourceConstrained))
	application, out, _ := newTestApplication(t, "", store, nil)
	application.dependencies.Config = doctorTestConfig()
	application.dependencies.Now = func() time.Time { return now }
	application.dependencies.Control = doctorControlFake{status: control.Status{ProcessID: 42, Phase: model.PhaseResourceConstrained, Version: "1.2.3"}}
	application.dependencies.Gaming = fakeGamingHost{inventory: host.GamingInventory{DesktopStatus: host.DesktopStatusRunning, DockerReachable: true}}
	application.dependencies.Doctor = &doctorInspectorFake{checks: []DoctorCheck{{Name: "environment", Healthy: true, Detail: "verified"}}}

	if code := application.Run(context.Background(), []string{"host", "doctor"}); code != ExitDegraded {
		t.Fatalf("doctor exit code = %d, want %d; output:\n%s", code, ExitDegraded, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] capacity-starved") {
		t.Fatalf("doctor did not expose resource-constrained total starvation:\n%s", out.String())
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
