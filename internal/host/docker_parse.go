package host

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseDesktopStatus(output []byte) (DesktopStatus, error) {
	normalized := strings.ToLower(strings.TrimSpace(string(output)))
	var document struct {
		Status string `json:"Status"`
	}
	if json.Unmarshal(output, &document) == nil && document.Status != "" {
		normalized = strings.ToLower(strings.TrimSpace(document.Status))
	}
	switch {
	case normalized == "running", strings.Contains(normalized, "is running"):
		return DesktopStatusRunning, nil
	case normalized == "stopped", strings.Contains(normalized, "is stopped"):
		return DesktopStatusStopped, nil
	case normalized == "starting", strings.Contains(normalized, "is starting"):
		return DesktopStatusStarting, nil
	case normalized == "stopping", strings.Contains(normalized, "is stopping"):
		return DesktopStatusStopping, nil
	default:
		return DesktopStatusUnknown, fmt.Errorf("unrecognized Docker Desktop status %q", strings.TrimSpace(string(output)))
	}
}
