package host

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"unicode/utf16"
)

func TestParseDesktopStatus(t *testing.T) {
	t.Parallel()
	tests := map[string]DesktopStatus{
		"Docker Desktop is running\r\n":          DesktopStatusRunning,
		"stopped":                                DesktopStatusStopped,
		"Docker Desktop is starting":             DesktopStatusStarting,
		"Docker Desktop is stopping":             DesktopStatusStopping,
		`{"SessionID":"one","Status":"running"}`: DesktopStatusRunning,
	}
	for input, want := range tests {
		t.Run(string(want), func(t *testing.T) {
			t.Parallel()
			got, err := parseDesktopStatus([]byte(input))
			if err != nil {
				t.Fatalf("parseDesktopStatus: %v", err)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestParseWSLDistributionsUTF16(t *testing.T) {
	t.Parallel()
	text := "Ubuntu-24.04\r\ndocker-desktop\r\n"
	units := utf16.Encode([]rune(text))
	encoded := []byte{0xff, 0xfe}
	for _, unit := range units {
		pair := make([]byte, 2)
		binary.LittleEndian.PutUint16(pair, unit)
		encoded = append(encoded, pair...)
	}
	want := []string{"Ubuntu-24.04", "docker-desktop"}
	if got := parseWSLDistributions(encoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

type fakeDesktop struct {
	status DesktopStatus
	stop   error
}

func (f *fakeDesktop) Status(context.Context) (DesktopStatus, error) { return f.status, nil }
func (f *fakeDesktop) Start(context.Context) error                   { return nil }
func (f *fakeDesktop) Stop(context.Context) error                    { return f.stop }

type fakeDocker struct {
	reachable  bool
	containers []Container
}

func (f *fakeDocker) EngineReachable(context.Context) (bool, error)   { return f.reachable, nil }
func (f *fakeDocker) Containers(context.Context) ([]Container, error) { return f.containers, nil }

type fakeWSL struct {
	running  []string
	shutdown error
}

func (f *fakeWSL) Running(context.Context) ([]string, error) { return f.running, nil }
func (f *fakeWSL) Shutdown(context.Context) error            { return f.shutdown }

func TestGamingManagerStopAllAttemptsBothSystems(t *testing.T) {
	t.Parallel()
	desktopErr := errors.New("desktop failure")
	wslErr := errors.New("wsl failure")
	manager := GamingManager{
		Desktop: &fakeDesktop{stop: desktopErr},
		Docker:  &fakeDocker{},
		WSL:     &fakeWSL{shutdown: wslErr},
	}
	err := manager.StopAll(context.Background())
	if !errors.Is(err, desktopErr) || !errors.Is(err, wslErr) {
		t.Fatalf("expected both failures, got %v", err)
	}
}

func TestGamingManagerVerificationRequiresAllPostconditions(t *testing.T) {
	t.Parallel()
	manager := GamingManager{
		Desktop: &fakeDesktop{status: DesktopStatusStopped},
		Docker:  &fakeDocker{reachable: false},
		WSL:     &fakeWSL{},
	}
	verification, err := manager.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !verification.Healthy() {
		t.Fatalf("expected healthy verification: %#v", verification)
	}
}

func TestControllerDesktopAdapterStatusUsesAllThreeSources(t *testing.T) {
	t.Parallel()
	adapter := ControllerDesktopAdapter{
		Desktop: &fakeDesktop{status: DesktopStatusRunning},
		Docker:  &fakeDocker{reachable: true},
		WSL:     &fakeWSL{running: []string{"docker-desktop"}},
	}
	status, err := adapter.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.DesktopRunning || !status.EngineReachable || status.RunningWSLCount != 1 {
		t.Fatalf("unexpected status: %#v", status)
	}
}
