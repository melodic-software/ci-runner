//go:build !windows

package host

func ProbePendingReboot() (PendingReboot, error) {
	return PendingReboot{}, ErrPendingRebootUnsupported
}
