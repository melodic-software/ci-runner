package scaleset

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	actionsscale "github.com/actions/scaleset"
)

// These tests run the client defaultOfficialFactory builds against a local
// fake of the GitHub API and the Actions service. Any host that is not
// github.com is treated as GHES, so the API lives at <host>/api/v3 and the
// Actions service and message queue URLs come from the fake's own responses.

const (
	fakeScaleSetID   = 42
	fakeScaleSetPath = "/tenant/_apis/runtime/runnerscalesets"
	fakeQueuePath    = "/queue"
)

var fakeAppPrivateKey = sync.OnceValue(func() []byte {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
})

type generatedKeySecretStore struct{}

func (generatedKeySecretStore) PrivateKey(context.Context, string) (SecretMaterial, error) {
	return NewSecretMaterial(fakeAppPrivateKey()), nil
}

// fakeGitHub serves the token exchange, the scale-set lookup, the message
// session, and the message queue. scaleSet and poll replace the default
// handlers for the two endpoints the tests fail on purpose.
type fakeGitHub struct {
	scaleSet http.HandlerFunc
	poll     http.HandlerFunc

	mu   sync.Mutex
	hits map[string]int
}

func (f *fakeGitHub) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/app/installations/12345/access_tokens":
		writeFakeJSON(w, http.StatusCreated, map[string]any{"token": "fake-installation-token", "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/orgs/melodic-software/actions/runners/registration-token":
		writeFakeJSON(w, http.StatusCreated, map[string]any{"token": "fake-registration-token", "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actions/runner-registration":
		writeFakeJSON(w, http.StatusOK, map[string]any{"url": "http://" + r.Host + "/tenant", "token": fakeAdminJWT()})
	case r.Method == http.MethodGet && r.URL.Path == fakeScaleSetPath:
		if f.scaleSet != nil {
			f.scaleSet(w, r)
			return
		}
		definition := fakeServerDefinition("")
		writeFakeJSON(w, http.StatusOK, map[string]any{"count": 1, "value": []actionsscale.RunnerScaleSet{{
			ID: fakeScaleSetID, Name: definition.ScaleSetName, RunnerGroupID: 1, Labels: buildLabels(definition),
			RunnerSetting: actionsscale.RunnerSetting{DisableUpdate: true},
		}}})
	case r.Method == http.MethodPost && r.URL.Path == fakeScaleSetPath+"/42/sessions":
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"sessionId": "6f1d6c1e-4f43-4c55-9f0e-3b0f1f7a2b11", "ownerName": "fake",
			"messageQueueUrl": "http://" + r.Host + fakeQueuePath, "messageQueueAccessToken": "fake-queue-token",
			"statistics": map[string]any{},
		})
	case r.Method == http.MethodDelete && r.URL.Path == fakeScaleSetPath+"/42/sessions/6f1d6c1e-4f43-4c55-9f0e-3b0f1f7a2b11":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == fakeQueuePath:
		if f.poll != nil {
			f.poll(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fakeAdminJWT builds an unsigned-in-effect token at run time: the SDK only
// parses its exp claim, unverified.
func fakeAdminJWT() string {
	segment := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	return segment(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		segment(map[string]int64{"exp": time.Now().Add(time.Hour).Unix()}) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("fake-signature"))
}

func fakeServerDefinition(serverURL string) Definition {
	return Definition{
		TargetID: "org", URL: serverURL + "/melodic-software", Scope: "organization",
		ClientID: "Iv23liFAKE", InstallationID: 12345, SecretID: "fake-secret",
		ScaleSetName: "melodic-ubuntu-24.04-x64", Labels: []string{"melodic-ubuntu-24.04-x64"},
	}
}

// newFakeServerClient starts fake and returns an OfficialClient built by the
// production factory, with no Factory override.
func newFakeServerClient(t *testing.T, fake *fakeGitHub, requestTimeout time.Duration) (*OfficialClient, Definition) {
	t.Helper()
	fake.hits = map[string]int{}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: requestTimeout, Secrets: generatedKeySecretStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client, fakeServerDefinition(server.URL)
}

func TestOfficialServerEnsureAndPollSucceed(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{}
	client, definition := newFakeServerClient(t, fake, 5*time.Second)
	identity, err := client.Ensure(context.Background(), definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ScaleSetID != fakeScaleSetID {
		t.Fatalf("identity = %#v", identity)
	}
	if _, err := client.Statistics(context.Background(), identity, 1); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialServerDoesNotRetryServerErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		fake := &fakeGitHub{scaleSet: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", status)
		}}
		client, definition := newFakeServerClient(t, fake, 5*time.Second)
		_, err := client.Ensure(context.Background(), definition, nil)
		if !IsKind(err, ErrorServer) {
			t.Fatalf("status %d: error = %v, want ErrorServer", status, err)
		}
		if hits := fake.count(fakeScaleSetPath); hits != 1 {
			t.Fatalf("status %d: scale-set requests = %d, want 1 (retries disabled)", status, hits)
		}
	}
}

func TestOfficialServerDoesNotRetryPollServerError(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{poll: func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}}
	client, definition := newFakeServerClient(t, fake, 5*time.Second)
	identity, err := client.Ensure(context.Background(), definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Statistics(context.Background(), identity, 1)
	if !IsKind(err, ErrorServer) {
		t.Fatalf("error = %v, want ErrorServer", err)
	}
	if hits := fake.count(fakeQueuePath); hits != 1 {
		t.Fatalf("poll requests = %d, want 1 (session inherits disabled retries)", hits)
	}
}

func TestOfficialServerTooManyRequestsCarriesRetryAfterWithoutRetry(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{scaleSet: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}}
	client, definition := newFakeServerClient(t, fake, 5*time.Second)
	start := time.Now()
	_, err := client.Ensure(context.Background(), definition, nil)
	elapsed := time.Since(start)
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorRateLimited || typed.StatusCode != http.StatusTooManyRequests || typed.RetryAfterSeconds != 7 {
		t.Fatalf("error = %#v, want ErrorRateLimited with RetryAfterSeconds 7", typed)
	}
	if hits := fake.count(fakeScaleSetPath); hits != 1 {
		t.Fatalf("scale-set requests = %d, want 1 (retries disabled)", hits)
	}
	if elapsed >= 7*time.Second {
		t.Fatalf("Ensure took %s; it waited out Retry-After instead of returning", elapsed)
	}
}

func TestOfficialServerRateLimitedForbiddenIsForbidden(t *testing.T) {
	t.Parallel()
	fake := &fakeGitHub{scaleSet: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "7")
		http.Error(w, "rate limited", http.StatusForbidden)
	}}
	client, definition := newFakeServerClient(t, fake, 5*time.Second)
	_, err := client.Ensure(context.Background(), definition, nil)
	var rateLimit *actionsscale.RateLimitError
	if !errors.As(err, &rateLimit) || rateLimit.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %v, want the SDK to classify the 403 as a rate limit", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorForbidden || typed.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %#v, want ErrorForbidden", typed)
	}
	if hits := fake.count(fakeScaleSetPath); hits != 1 {
		t.Fatalf("scale-set requests = %d, want 1 (retries disabled)", hits)
	}
}

func TestOfficialServerPollIsCutOffAtRequestTimeout(t *testing.T) {
	t.Parallel()
	const requestTimeout = 300 * time.Millisecond
	var polls atomic.Int32
	fake := &fakeGitHub{poll: func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			w.WriteHeader(http.StatusAccepted)
		}
	}}
	client, definition := newFakeServerClient(t, fake, requestTimeout)
	identity, err := client.Ensure(context.Background(), definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = client.Statistics(context.Background(), identity, 1)
	elapsed := time.Since(start)
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorTransport || typed.StatusCode != 0 {
		t.Fatalf("error = %#v, want ErrorTransport from the client timeout", err)
	}
	if elapsed < requestTimeout || elapsed > 5*time.Second {
		t.Fatalf("poll returned after %s, want about %s (not MessagePollTimeout %s)", elapsed, requestTimeout, actionsscale.MessagePollTimeout)
	}
	if got := polls.Load(); got != 1 {
		t.Fatalf("poll requests = %d, want 1", got)
	}
}
