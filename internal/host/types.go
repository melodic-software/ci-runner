package host

import "context"

type DesktopStatus string

const (
	DesktopStatusUnknown  DesktopStatus = "unknown"
	DesktopStatusRunning  DesktopStatus = "running"
	DesktopStatusStarting DesktopStatus = "starting"
	DesktopStatusStopping DesktopStatus = "stopping"
	DesktopStatusStopped  DesktopStatus = "stopped"
)

type Container struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Status  string            `json:"status"`
	Labels  map[string]string `json:"labels,omitempty"`
	Managed bool              `json:"managed"`
}

type GamingInventory struct {
	DesktopStatus        DesktopStatus `json:"desktopStatus"`
	DockerReachable      bool          `json:"dockerReachable"`
	CIContainers         []Container   `json:"ciContainers,omitempty"`
	NonCIContainers      []Container   `json:"nonCiContainers,omitempty"`
	RunningDistributions []string      `json:"runningDistributions,omitempty"`
	Problems             []string      `json:"problems,omitempty"`
}

type GamingVerification struct {
	DesktopStopped       bool     `json:"desktopStopped"`
	DockerUnreachable    bool     `json:"dockerUnreachable"`
	NoRunningWSL         bool     `json:"noRunningWsl"`
	RunningDistributions []string `json:"runningDistributions,omitempty"`
}

func (v GamingVerification) Healthy() bool {
	return v.DesktopStopped && v.DockerUnreachable && v.NoRunningWSL
}

type DesktopManager interface {
	Status(context.Context) (DesktopStatus, error)
	Start(context.Context) error
	Stop(context.Context) error
}

type DockerInspector interface {
	EngineReachable(context.Context) (bool, error)
	Containers(context.Context) ([]Container, error)
}

type WSLManager interface {
	Running(context.Context) ([]string, error)
	Shutdown(context.Context) error
}

type GamingHost interface {
	Inventory(context.Context) GamingInventory
	StopAll(context.Context) error
	Verify(context.Context) (GamingVerification, error)
}
