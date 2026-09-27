//go:build windows

package controller

import "golang.org/x/sys/windows"

func processCPUSeconds() float64 {
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user) != nil {
		return 0
	}
	// Kernel and user times are durations in 100ns units, not dates, so Filetime.Nanoseconds does not apply.
	ticks := uint64(kernel.HighDateTime)<<32 | uint64(kernel.LowDateTime)
	ticks += uint64(user.HighDateTime)<<32 | uint64(user.LowDateTime)
	return float64(ticks) / 1e7
}
