//go:build windows

package childprocess

import (
	"context"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCommandContextPreventsConsoleWindow(t *testing.T) {
	t.Parallel()
	cmd := CommandContext(context.Background(), `fixture.exe`, "one", "two")
	if cmd.SysProcAttr == nil {
		t.Fatal("Windows process attributes are nil")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is false")
	}
	if got := cmd.SysProcAttr.CreationFlags & windows.CREATE_NO_WINDOW; got != windows.CREATE_NO_WINDOW {
		t.Errorf("CreationFlags = %#x, missing CREATE_NO_WINDOW (%#x)", cmd.SysProcAttr.CreationFlags, windows.CREATE_NO_WINDOW)
	}
	if want := []string{`fixture.exe`, "one", "two"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("Args = %#v, want %#v", cmd.Args, want)
	}
}
