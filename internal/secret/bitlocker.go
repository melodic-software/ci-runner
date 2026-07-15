package secret

import (
	"encoding/json"
	"fmt"
	"strings"
)

type bitLockerStatus struct {
	VolumeStatus     string `json:"VolumeStatus"`
	ProtectionStatus string `json:"ProtectionStatus"`
}

func parseBitLockerStatus(value []byte) error {
	var status bitLockerStatus
	if err := json.Unmarshal(value, &status); err != nil {
		return fmt.Errorf("decode BitLocker status: %w", err)
	}
	if !strings.EqualFold(status.VolumeStatus, "FullyEncrypted") {
		return fmt.Errorf("BitLocker volume status is %q, not FullyEncrypted", status.VolumeStatus)
	}
	if !strings.EqualFold(status.ProtectionStatus, "On") {
		return fmt.Errorf("BitLocker protection status is %q, not On", status.ProtectionStatus)
	}
	return nil
}
