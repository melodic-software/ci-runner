package healthwatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebhookAlerterPostsSlackCompatiblePayload(t *testing.T) {
	t.Parallel()
	var received struct {
		Text string `json:"text"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", request.Method)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Fatal(err)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	alerter := NewWebhookAlerter(server.URL)
	err := alerter.Send(context.Background(), Result{
		CheckedAt: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		Findings: []Finding{{
			Code:    "stale-heartbeat",
			Message: "heartbeat is stale",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(received.Text, "stale-heartbeat") || !strings.Contains(received.Text, "heartbeat is stale") {
		t.Fatalf("payload = %q", received.Text)
	}
}

func TestAlertFingerprintIgnoresVolatileMessageText(t *testing.T) {
	t.Parallel()
	first := Result{Healthy: false, Findings: []Finding{{
		Code: "stale-heartbeat", Message: "age=30s",
	}}}
	second := Result{Healthy: false, Findings: []Finding{{
		Code: "stale-heartbeat", Message: "age=45s",
	}}}
	if alertFingerprint(first) != alertFingerprint(second) {
		t.Fatalf("fingerprints differ for same finding code with different messages")
	}
}

func TestShouldSendAlertHonorsCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	result := Result{Healthy: false, Findings: []Finding{{Code: "stale-heartbeat", Message: "stale"}}}
	last := now.Add(-time.Minute)
	sidecar := Sidecar{
		LastAlertFingerprint: alertFingerprint(result),
		LastAlertAt:          &last,
	}
	if ShouldSendAlert(sidecar, result, now, 15*time.Minute) {
		t.Fatal("expected cooldown to suppress duplicate alert")
	}
}
