package scaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	actionsscale "github.com/actions/scaleset"
	"github.com/google/uuid"
)

func TestOfficialEnsureCreatesPersistentDisableUpdateScaleSet(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	factory := &fakeOfficialFactory{api: api}
	client := newOfficialForTest(t, factory)
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ScaleSetID != 42 || identity.ListenerID != api.session.session.SessionID.String() {
		t.Fatalf("identity = %#v", identity)
	}
	created := api.createdScaleSet()
	if created == nil || !created.RunnerSetting.DisableUpdate || created.Name != "melodic-ubuntu-24.04-x64" || created.RunnerGroupID != 7 {
		t.Fatalf("created scale set = %#v", created)
	}
	if len(created.Labels) != 1 || created.Labels[0].Name != created.Name {
		t.Fatalf("labels = %#v", created.Labels)
	}
	if factory.secret() != "test-private-key" {
		t.Fatalf("factory secret = %q", factory.secret())
	}
}

func TestOfficialEnsureConvergesUpdateAndDoesNotDeleteOnClose(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	api.existing = &actionsscale.RunnerScaleSet{
		ID: 42, Name: "melodic-ubuntu-24.04-x64", RunnerGroupID: 7,
		Labels:        []actionsscale.Label{{Name: "wrong", Type: "System"}},
		RunnerSetting: actionsscale.RunnerSetting{DisableUpdate: false},
	}
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	if _, err := client.Ensure(context.Background(), testDefinition(), nil); err != nil {
		t.Fatal(err)
	}
	updated := api.updatedScaleSet()
	if updated == nil || !updated.RunnerSetting.DisableUpdate || updated.Labels[0].Name != "melodic-ubuntu-24.04-x64" {
		t.Fatalf("updated = %#v", updated)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.deleteCount() != 0 {
		t.Fatal("normal close deleted persistent scale set")
	}
	if err := client.DeleteScaleSet(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if api.deleteCount() != 1 {
		t.Fatal("explicit uninstall did not delete scale set")
	}
}

func TestOfficialStatisticsReportsCapacityAcknowledgesAcquiresAndTracksJobs(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID:            9,
		Statistics:           &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
		JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 101}}},
		JobStartedMessages:   []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	})
	stats, err := client.Statistics(context.Background(), identity, 3)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalAssignedJobs != 3 || api.session.maxCapacity != 3 {
		t.Fatalf("stats=%#v max=%d", stats, api.session.maxCapacity)
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get delete acquire]" {
		t.Fatalf("message order = %v", got)
	}
	if jobID, ok := client.ActiveJob("org", "runner-1"); !ok || jobID != "job-1" {
		t.Fatalf("active job = %q %v", jobID, ok)
	}

	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID:            10,
		Statistics:           &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
		JobCompletedMessages: []*actionsscale.JobCompleted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	})
	if _, err := client.Statistics(context.Background(), identity, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ActiveJob("org", "runner-1"); ok {
		t.Fatal("completed job remained active")
	}
}

func TestOfficialPersistenceFailurePreventsAcquireAndMessageAcknowledgement(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	persistErr := errors.New("jobs.json unavailable")
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Events: failingJobEventSink{err: persistErr},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID: 11, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 101}}},
		JobStartedMessages:   []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	})
	_, err = client.Statistics(context.Background(), identity, 1)
	if !errors.Is(err, persistErr) || !Retryable(err) {
		t.Fatalf("error = %v, retryable=%v", err, Retryable(err))
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get]" {
		t.Fatalf("message was acquired or acknowledged after persistence failure: %v", got)
	}
}

func TestOfficialPersistencePrecedesDeleteAndDeleteFailureNeverAcquires(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	deleteErr := errors.New("delete failed")
	api.session.deleteErr = deleteErr
	events := &recordingJobEventSink{}
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Events: events,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	message := &actionsscale.RunnerScaleSetMessage{
		MessageID: 12, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 101}}},
		JobStartedMessages:   []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	}
	api.session.messages = append(api.session.messages, message)
	if _, err := client.Statistics(context.Background(), identity, 1); !errors.Is(err, deleteErr) {
		t.Fatalf("delete error = %v", err)
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get delete]" {
		t.Fatalf("delete failure call order = %v", got)
	}
	api.session.deleteErr = nil
	api.session.messages = append(api.session.messages, message)
	if _, err := client.Statistics(context.Background(), identity, 1); err != nil {
		t.Fatal(err)
	}
	if events.started != 2 {
		t.Fatalf("redelivered lifecycle persistence calls = %d, want two idempotent upserts", events.started)
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get delete get delete acquire]" {
		t.Fatalf("redelivery call order = %v", got)
	}
}

func TestOfficialAcquireFailureRetainsPersistedActiveJobState(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	api.session.acquireErr = errors.New("acquire failed after irreversible delete")
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID: 13, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
		JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 101}}},
		JobStartedMessages:   []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	})
	if _, err := client.Statistics(context.Background(), identity, 1); !errors.Is(err, api.session.acquireErr) {
		t.Fatalf("acquire error = %v", err)
	}
	if jobID, active := client.ActiveJob("org", "runner-1"); !active || jobID != "job-1" {
		t.Fatalf("active job after acquire failure = %q %t", jobID, active)
	}
}

func TestOfficialJITConfigIsValidatedAndRedacted(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	identity, _ := client.Ensure(context.Background(), testDefinition(), nil)
	jit, err := client.CreateJITConfig(context.Background(), identity, "runner-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(jit.Reveal()); got != "encoded-jit" || jit.RunnerID() != 99 {
		t.Fatalf("JIT = %q runnerID=%d", got, jit.RunnerID())
	}
	if got := fmt.Sprintf("%v", jit); got != "[REDACTED JIT CONFIG]" {
		t.Fatalf("formatted JIT = %q", got)
	}
	if err := client.RemoveRunner(context.Background(), "org", jit.RunnerID()); err != nil {
		t.Fatal(err)
	}
	if len(api.runnerRemovals) != 1 || api.runnerRemovals[0] != 99 {
		t.Fatalf("runner removals = %#v", api.runnerRemovals)
	}
}

func TestOfficialErrorClassification(t *testing.T) {
	t.Parallel()
	tests := map[int]ErrorKind{
		401: ErrorUnauthorized, 403: ErrorForbidden, 404: ErrorNotFound,
		409: ErrorConflict, 429: ErrorRateLimited, 500: ErrorServer,
	}
	for status, want := range tests {
		err := translateOfficialError("test", fmt.Errorf("request failed(status=\"%d Example\")", status))
		var typed *Error
		if !errors.As(err, &typed) || typed.Kind != want || typed.StatusCode != status {
			t.Errorf("status %d => %#v, want %s", status, typed, want)
		}
	}
}

func TestOfficialDeadlineIsRetryableTransportFailure(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.getMessageErr = context.DeadlineExceeded
	_, err = client.Statistics(context.Background(), identity, 1)
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorTransport || !Retryable(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline classification = %#v, retryable=%v", typed, Retryable(err))
	}
}

func TestOfficialRetryAfterHeaderReachesTypedRetryPolicy(t *testing.T) {
	t.Parallel()
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"37"}}}
	_, sourceErr := officialRetryErrorHandler(response, errors.New("rate limited"), 1)
	err := translateOfficialError("poll", fmt.Errorf("wrapped official response: %w", sourceErr))
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorRateLimited || typed.StatusCode != http.StatusTooManyRequests || typed.RetryAfterSeconds != 37 || !Retryable(err) {
		t.Fatalf("rate-limit classification = %#v, retryable=%v", typed, Retryable(err))
	}
}

func newOfficialForTest(t *testing.T, factory officialFactory) *OfficialClient {
	t.Helper()
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testDefinition() Definition {
	return Definition{
		TargetID: "org", URL: "https://github.com/melodic-software", Scope: "organization",
		ClientID: "Iv23liABCDEF1234", InstallationID: 12345, SecretID: "melodic-org-host",
		RunnerGroup: "ci-local-melo-desk-001", ScaleSetName: "melodic-ubuntu-24.04-x64",
		Labels: []string{"melodic-ubuntu-24.04-x64"},
	}
}

type fakeSecretStore struct{}

func (fakeSecretStore) PrivateKey(context.Context, string) (SecretMaterial, error) {
	return NewSecretMaterial([]byte("test-private-key")), nil
}

type failingJobEventSink struct{ err error }

func (s failingJobEventSink) JobStarted(context.Context, string, string, string) error { return s.err }
func (s failingJobEventSink) JobCompleted(context.Context, string, string, string, string) error {
	return s.err
}

type recordingJobEventSink struct{ started int }

func (s *recordingJobEventSink) JobStarted(context.Context, string, string, string) error {
	s.started++
	return nil
}
func (*recordingJobEventSink) JobCompleted(context.Context, string, string, string, string) error {
	return nil
}

type fakeOfficialFactory struct {
	mu       sync.Mutex
	api      officialAPI
	keyValue string
}

func (f *fakeOfficialFactory) New(_ context.Context, _ Definition, key SecretMaterial) (officialAPI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyValue = string(key.Reveal())
	return f.api, nil
}
func (f *fakeOfficialFactory) secret() string { f.mu.Lock(); defer f.mu.Unlock(); return f.keyValue }

type fakeOfficialAPI struct {
	mu             sync.Mutex
	existing       *actionsscale.RunnerScaleSet
	created        *actionsscale.RunnerScaleSet
	updated        *actionsscale.RunnerScaleSet
	deletes        int
	runnerRemovals []int64
	session        *fakeOfficialSession
}

func newFakeOfficialAPI() *fakeOfficialAPI {
	return &fakeOfficialAPI{session: &fakeOfficialSession{session: actionsscale.RunnerScaleSetSession{
		SessionID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Statistics: &actionsscale.RunnerScaleSetStatistic{},
	}}}
}

func (*fakeOfficialAPI) GetRunnerGroupByName(context.Context, string) (*actionsscale.RunnerGroup, error) {
	return &actionsscale.RunnerGroup{ID: 7, Name: "ci-local-melo-desk-001"}, nil
}
func (a *fakeOfficialAPI) GetRunnerScaleSet(context.Context, int, string) (*actionsscale.RunnerScaleSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneScaleSet(a.existing), nil
}
func (a *fakeOfficialAPI) GetRunnerScaleSetByID(context.Context, int) (*actionsscale.RunnerScaleSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneScaleSet(a.existing), nil
}
func (a *fakeOfficialAPI) CreateRunnerScaleSet(_ context.Context, scaleSet *actionsscale.RunnerScaleSet) (*actionsscale.RunnerScaleSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	created := *scaleSet
	created.ID = 42
	a.created = cloneScaleSet(&created)
	a.existing = cloneScaleSet(&created)
	return cloneScaleSet(&created), nil
}
func (a *fakeOfficialAPI) UpdateRunnerScaleSet(_ context.Context, _ int, scaleSet *actionsscale.RunnerScaleSet) (*actionsscale.RunnerScaleSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updated = cloneScaleSet(scaleSet)
	a.existing = cloneScaleSet(scaleSet)
	return cloneScaleSet(scaleSet), nil
}
func (a *fakeOfficialAPI) DeleteRunnerScaleSet(context.Context, int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deletes++
	return nil
}
func (a *fakeOfficialAPI) MessageSession(context.Context, int, string) (officialSession, error) {
	return a.session, nil
}
func (*fakeOfficialAPI) GenerateJITConfig(_ context.Context, setting *actionsscale.RunnerScaleSetJitRunnerSetting, _ int) (*actionsscale.RunnerScaleSetJitRunnerConfig, error) {
	return &actionsscale.RunnerScaleSetJitRunnerConfig{Runner: &actionsscale.RunnerReference{ID: 99, Name: setting.Name}, EncodedJITConfig: "encoded-jit"}, nil
}
func (a *fakeOfficialAPI) RemoveRunner(_ context.Context, runnerID int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.runnerRemovals = append(a.runnerRemovals, runnerID)
	return nil
}
func (a *fakeOfficialAPI) createdScaleSet() *actionsscale.RunnerScaleSet {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneScaleSet(a.created)
}
func (a *fakeOfficialAPI) updatedScaleSet() *actionsscale.RunnerScaleSet {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneScaleSet(a.updated)
}
func (a *fakeOfficialAPI) deleteCount() int { a.mu.Lock(); defer a.mu.Unlock(); return a.deletes }

type fakeOfficialSession struct {
	mu            sync.Mutex
	session       actionsscale.RunnerScaleSetSession
	messages      []*actionsscale.RunnerScaleSetMessage
	maxCapacity   int
	calls         []string
	getMessageErr error
	deleteErr     error
	acquireErr    error
}

func (s *fakeOfficialSession) Session() actionsscale.RunnerScaleSetSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}
func (s *fakeOfficialSession) GetMessage(_ context.Context, _ int, max int) (*actionsscale.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "get")
	s.maxCapacity = max
	if s.getMessageErr != nil {
		return nil, s.getMessageErr
	}
	if len(s.messages) == 0 {
		return nil, nil
	}
	message := s.messages[0]
	s.messages = s.messages[1:]
	return message, nil
}
func (s *fakeOfficialSession) DeleteMessage(context.Context, int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "delete")
	return s.deleteErr
}
func (s *fakeOfficialSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "acquire")
	return append([]int64(nil), ids...), s.acquireErr
}
func (*fakeOfficialSession) Close(context.Context) error { return nil }
func (s *fakeOfficialSession) callOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func cloneScaleSet(input *actionsscale.RunnerScaleSet) *actionsscale.RunnerScaleSet {
	if input == nil {
		return nil
	}
	copy := *input
	copy.Labels = append([]actionsscale.Label(nil), input.Labels...)
	return &copy
}

var _ officialAPI = (*fakeOfficialAPI)(nil)
var _ officialSession = (*fakeOfficialSession)(nil)
