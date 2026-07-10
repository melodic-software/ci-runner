//go:build windows

package host

func NewControllerDesktopAdapter() ControllerDesktopAdapter {
	return ControllerDesktopAdapter{
		Desktop: DockerDesktopCLI{},
		Docker:  DockerCLIInspector{},
		WSL:     WSLCLI{},
	}
}
