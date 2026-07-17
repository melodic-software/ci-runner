package host

import "errors"

var ErrPendingRebootUnsupported = errors.New("pending-reboot inspection requires Windows")

// PendingReboot reports the standard Windows servicing signals that remain set
// while an installed update still needs a restart the host never performs
// automatically.
type PendingReboot struct {
	ComponentServicing   bool
	FileRenameOperations bool
	WindowsUpdate        bool
}

func (p PendingReboot) Pending() bool {
	return p.ComponentServicing || p.FileRenameOperations || p.WindowsUpdate
}

func (p PendingReboot) Signals() []string {
	signals := make([]string, 0, 3)
	if p.ComponentServicing {
		signals = append(signals, "component-servicing")
	}
	if p.FileRenameOperations {
		signals = append(signals, "pending-file-renames")
	}
	if p.WindowsUpdate {
		signals = append(signals, "windows-update")
	}
	return signals
}
