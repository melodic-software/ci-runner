package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type dockerPSRecord struct {
	ID     string `json:"ID"`
	Names  string `json:"Names"`
	Image  string `json:"Image"`
	Status string `json:"Status"`
	Labels string `json:"Labels"`
}

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

func parseDockerPS(output []byte) ([]Container, error) {
	var containers []Container
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record dockerPSRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode docker ps record: %w", err)
		}
		labels := parseDockerLabels(record.Labels)
		containers = append(containers, Container{
			ID:      record.ID,
			Name:    record.Names,
			Image:   record.Image,
			Status:  record.Status,
			Labels:  labels,
			Managed: strings.EqualFold(labels[managedContainerLabel], "true"),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read docker ps output: %w", err)
	}
	return containers, nil
}

func parseDockerLabels(value string) map[string]string {
	labels := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		key, val, found := strings.Cut(item, "=")
		if !found {
			labels[key] = ""
			continue
		}
		labels[key] = val
	}
	return labels
}
