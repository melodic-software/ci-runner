//go:build windows

package childprocess

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
