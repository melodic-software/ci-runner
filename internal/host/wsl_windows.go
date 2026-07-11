//go:build windows

package host

import "context"

type WSLCLI struct {
	Runner         CommandRunner
	executablePath string
}

func (w WSLCLI) executable() (string, error) {
	if w.executablePath != "" {
		return w.executablePath, nil
	}
	return trustedSystemExecutable("wsl.exe")
}

func (w WSLCLI) runner() CommandRunner {
	if w.Runner == nil {
		return ExecCommandRunner{}
	}
	return w.Runner
}

func (w WSLCLI) Running(ctx context.Context) ([]string, error) {
	executable, err := w.executable()
	if err != nil {
		return nil, err
	}
	out, err := w.runner().Run(ctx, executable, "--list", "--running", "--quiet")
	if err != nil {
		return nil, err
	}
	return parseWSLDistributions(out), nil
}

func (w WSLCLI) Shutdown(ctx context.Context) error {
	executable, err := w.executable()
	if err != nil {
		return err
	}
	_, err = w.runner().Run(ctx, executable, "--shutdown")
	return err
}

func NewPlatformGamingHost() GamingHost {
	desktop := DockerDesktopCLI{}
	return GamingManager{
		Desktop: desktop,
		Docker:  DockerCLIInspector{},
		WSL:     WSLCLI{},
	}
}
