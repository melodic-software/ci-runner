//go:build windows

package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/melodic-software/ci-runner/internal/model"
)

var (
	monitorKernel32          = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemPowerStatus = monitorKernel32.NewProc("GetSystemPowerStatus")
	procGlobalMemoryStatusEx = monitorKernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = monitorKernel32.NewProc("GetSystemTimes")
)

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

type WindowsPowerMonitor struct{}

func (WindowsPowerMonitor) Snapshot(ctx context.Context) (model.PowerSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return model.PowerSnapshot{}, err
	}
	var status systemPowerStatus
	result, _, callErr := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&status)))
	if result == 0 {
		return model.PowerSnapshot{}, fmt.Errorf("GetSystemPowerStatus: %w", monitorCallError(callErr))
	}
	if status.ACLineStatus != 0 && status.ACLineStatus != 1 {
		return model.PowerSnapshot{}, fmt.Errorf("Windows reported unknown AC line status %#x", status.ACLineStatus)
	}
	return model.PowerSnapshot{ACConnected: status.ACLineStatus == 1, ObservedAt: time.Now().UTC()}, nil
}

type memoryStatusEx struct {
	Length                   uint32
	MemoryLoad               uint32
	TotalPhysical            uint64
	AvailablePhysical        uint64
	TotalPageFile            uint64
	AvailablePageFile        uint64
	TotalVirtual             uint64
	AvailableVirtual         uint64
	AvailableExtendedVirtual uint64
}

type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (f filetime) ticks() uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// WindowsResourceMonitor samples GetSystemTimes over a short interval. The
// controller owns the longer policy observation/hysteresis windows.
type WindowsResourceMonitor struct {
	mu             sync.Mutex
	SampleInterval time.Duration
}

func (m *WindowsResourceMonitor) Snapshot(ctx context.Context) (model.ResourceSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return model.ResourceSnapshot{}, err
	}
	var memory memoryStatusEx
	memory.Length = uint32(unsafe.Sizeof(memory))
	result, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&memory)))
	if result == 0 {
		return model.ResourceSnapshot{}, fmt.Errorf("GlobalMemoryStatusEx: %w", monitorCallError(callErr))
	}
	first, err := readSystemTimes()
	if err != nil {
		return model.ResourceSnapshot{}, err
	}
	interval := m.SampleInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return model.ResourceSnapshot{}, ctx.Err()
	case <-timer.C:
	}
	second, err := readSystemTimes()
	if err != nil {
		return model.ResourceSnapshot{}, err
	}
	total := (second.kernel - first.kernel) + (second.user - first.user)
	idle := second.idle - first.idle
	if total == 0 || idle > total {
		return model.ResourceSnapshot{}, errors.New("GetSystemTimes returned a non-increasing sample")
	}
	cpu := float64(total-idle) * 100 / float64(total)
	return model.ResourceSnapshot{
		TotalMemoryBytes:      memory.TotalPhysical,
		AvailableMemoryBytes:  memory.AvailablePhysical,
		CPUUtilizationPercent: cpu,
	}, nil
}

type systemTimes struct{ idle, kernel, user uint64 }

func readSystemTimes() (systemTimes, error) {
	var idle, kernel, user filetime
	result, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if result == 0 {
		return systemTimes{}, fmt.Errorf("GetSystemTimes: %w", monitorCallError(callErr))
	}
	return systemTimes{idle: idle.ticks(), kernel: kernel.ticks(), user: user.ticks()}, nil
}

func monitorCallError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("Windows API call failed")
	}
	return err
}
