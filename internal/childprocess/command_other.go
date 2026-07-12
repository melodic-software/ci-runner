//go:build !windows

package childprocess

import "os/exec"

func configure(_ *exec.Cmd) {}
