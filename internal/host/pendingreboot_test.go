package host

import (
	"reflect"
	"testing"
)

func TestPendingRebootNamesEachFiredSignal(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		state       PendingReboot
		wantPending bool
		wantSignals []string
	}{
		"none": {state: PendingReboot{}, wantSignals: []string{}},
		"component-servicing": {
			state:       PendingReboot{ComponentServicing: true},
			wantPending: true,
			wantSignals: []string{"component-servicing"},
		},
		"file-renames": {
			state:       PendingReboot{FileRenameOperations: true},
			wantPending: true,
			wantSignals: []string{"pending-file-renames"},
		},
		"windows-update": {
			state:       PendingReboot{WindowsUpdate: true},
			wantPending: true,
			wantSignals: []string{"windows-update"},
		},
		"all": {
			state:       PendingReboot{ComponentServicing: true, FileRenameOperations: true, WindowsUpdate: true},
			wantPending: true,
			wantSignals: []string{"component-servicing", "pending-file-renames", "windows-update"},
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := test.state.Pending(); got != test.wantPending {
				t.Fatalf("Pending() = %t, want %t", got, test.wantPending)
			}
			if got := test.state.Signals(); !reflect.DeepEqual(got, test.wantSignals) {
				t.Fatalf("Signals() = %#v, want %#v", got, test.wantSignals)
			}
		})
	}
}
