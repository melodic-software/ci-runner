//go:build windows

package host

func NewControllerDesktopAdapter() ControllerDesktopAdapter {
	return ControllerDesktopAdapter{
		Desktop: DockerDesktopCLI{},
		Docker:  DockerEngineInspector{},
		WSL:     WSLCLI{},
	}
}
