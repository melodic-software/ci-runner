package healthwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type WebhookAlerter struct {
	URL    string
	Client *http.Client
}

func NewWebhookAlerter(url string) WebhookAlerter {
	client := http.DefaultClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	} else if client.Timeout == 0 {
		cloned := *client
		cloned.Timeout = 15 * time.Second
		client = &cloned
	}
	return WebhookAlerter{URL: strings.TrimSpace(url), Client: client}
}

func (a WebhookAlerter) Enabled() bool {
	return a.URL != ""
}

func (a WebhookAlerter) Send(ctx context.Context, result Result) error {
	if !a.Enabled() {
		return nil
	}
	var builder strings.Builder
	builder.WriteString("ci-runner external health watch detected ")
	builder.WriteString(fmt.Sprintf("%d issue(s) at %s:\n", len(result.Findings), result.CheckedAt.Format(time.RFC3339)))
	for _, finding := range result.Findings {
		builder.WriteString("- ")
		builder.WriteString(finding.Code)
		builder.WriteString(": ")
		builder.WriteString(finding.Message)
		builder.WriteByte('\n')
	}
	payload, err := json.Marshal(map[string]string{"text": strings.TrimSpace(builder.String())})
	if err != nil {
		return fmt.Errorf("encode health watch alert payload: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create health watch alert request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.Client.Do(request)
	if err != nil {
		return fmt.Errorf("post health watch alert: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("post health watch alert: unexpected status %s", response.Status)
	}
	return nil
}

func ShouldSendAlert(sidecar Sidecar, result Result, now time.Time, cooldown time.Duration) bool {
	if result.Healthy {
		return false
	}
	fingerprint := alertFingerprint(result)
	if sidecar.LastAlertFingerprint == fingerprint && sidecar.LastAlertAt != nil && now.Sub(*sidecar.LastAlertAt) < cooldown {
		return false
	}
	return true
}

func alertFingerprint(result Result) string {
	var builder strings.Builder
	for _, finding := range result.Findings {
		builder.WriteString(finding.Code)
		builder.WriteByte('|')
		builder.WriteString(finding.Message)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func RecordAlert(sidecar Sidecar, result Result, now time.Time) Sidecar {
	sidecar.LastAlertFingerprint = alertFingerprint(result)
	sidecar.LastAlertAt = &now
	return sidecar
}

func (c *Checker) CheckAndAlert(ctx context.Context, sendAlert bool) (Result, error) {
	result, err := c.Check(ctx)
	if err != nil {
		return Result{}, err
	}
	if !sendAlert || result.Healthy {
		return result, nil
	}
	sidecar, err := c.deps.Sidecar.Load(ctx)
	if err != nil {
		return result, fmt.Errorf("load health watch sidecar for alert deduplication: %w", err)
	}
	cooldown := c.deps.Config.HealthWatchdog.AlertCooldown.Duration
	if !ShouldSendAlert(sidecar, result, c.deps.Now(), cooldown) {
		return result, nil
	}
	if err := c.deps.Alert.Send(ctx, result); err != nil {
		return result, err
	}
	sidecar = RecordAlert(sidecar, result, c.deps.Now())
	if err := c.deps.Sidecar.Save(ctx, sidecar); err != nil {
		return result, errors.Join(errors.New("persist health watch alert receipt"), err)
	}
	return result, nil
}
