package app

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingDoctorBitLocker struct {
	calls int
	err   error
}

func (v *recordingDoctorBitLocker) VerifyProtected(context.Context, string) error {
	v.calls++
	return v.err
}

func TestLocalDoctorInspectorSkipsBitLockerByDefaultWithoutCallingVerifier(t *testing.T) {
	t.Parallel()
	verifier := &recordingDoctorBitLocker{err: errors.New("must not be called")}
	inspector := &LocalDoctorInspector{BitLocker: verifier}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{}), "bitlocker")

	if verifier.calls != 0 {
		t.Fatalf("default doctor invoked the UAC-capable BitLocker verifier %d time(s)", verifier.calls)
	}
	if !check.Skipped || !strings.Contains(check.Detail, "--include-elevated") {
		t.Fatalf("default BitLocker check = %#v, want an explicit non-failing skip", check)
	}
}

func TestLocalDoctorInspectorRunsAndEnforcesBitLockerWhenExplicitlyIncluded(t *testing.T) {
	t.Parallel()
	verifier := &recordingDoctorBitLocker{err: errors.New("protection is off")}
	inspector := &LocalDoctorInspector{BitLocker: verifier}

	check := doctorCheckNamed(t, inspector.Inspect(context.Background(), DoctorInspection{IncludeElevated: true}), "bitlocker")

	if verifier.calls != 1 {
		t.Fatalf("opted-in doctor invoked the BitLocker verifier %d time(s), want 1", verifier.calls)
	}
	if check.Skipped || check.Healthy || !strings.Contains(check.Detail, "protection is off") {
		t.Fatalf("opted-in BitLocker failure was weakened: %#v", check)
	}
}

func doctorCheckNamed(t *testing.T, checks []DoctorCheck, name string) DoctorCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor check %q not found in %#v", name, checks)
	return DoctorCheck{}
}
