//go:build windows

package host

import "testing"

func TestLocalMachineKeyExistsDistinguishesPresentFromAbsent(t *testing.T) {
	t.Parallel()
	exists, err := localMachineKeyExists(`SOFTWARE\Microsoft\Windows\CurrentVersion`)
	if err != nil || !exists {
		t.Fatalf("well-known key: exists=%t err=%v, want present without error", exists, err)
	}
	exists, err = localMachineKeyExists(`SOFTWARE\melodic-software\ci-runner\pending-reboot-probe-key-that-must-not-exist`)
	if err != nil || exists {
		t.Fatalf("absent key: exists=%t err=%v, want absent without error", exists, err)
	}
}

func TestProbePendingRebootReadsAllSignalsWithoutElevation(t *testing.T) {
	t.Parallel()
	if _, err := ProbePendingReboot(); err != nil {
		t.Fatalf("ProbePendingReboot() = %v, want a non-elevated read of every signal to succeed", err)
	}
}
