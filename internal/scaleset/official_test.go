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
	observer := &recordingJobEventObserver{}
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID:            9,
		Statistics:           &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
		JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 101}}},
		JobStartedMessages: []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{
			JobID: "job-1", RunnerAssignTime: time.Now().Add(-time.Second),
		}}},
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
	if len(observer.starts) != 1 || observer.starts[0] < 900*time.Millisecond || observer.starts[0] > 5*time.Second {
		t.Fatalf("job-start visibility observations = %v", observer.starts)
	}

	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID:            10,
		Statistics:           &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
		JobCompletedMessages: []*actionsscale.JobCompleted{{RunnerID: 41, RunnerName: "runner-1", Result: "Succeeded", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
	})
	if _, err := client.Statistics(context.Background(), identity, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ActiveJob("org", "runner-1"); ok {
		t.Fatal("completed job remained active")
	}
}

func TestOfficialUnassignedCanceledCompletionAcknowledgesAndLaterCapacityRecovers(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	events := &recordingJobEventSink{}
	observer := &recordingJobEventObserver{}
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Events: events, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages,
		&actionsscale.RunnerScaleSetMessage{
			MessageID: 20, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 0},
			JobCompletedMessages: []*actionsscale.JobCompleted{{
				Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "canceled-before-assignment"},
			}},
		},
		&actionsscale.RunnerScaleSetMessage{
			MessageID: 21, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
			JobAvailableMessages: []*actionsscale.JobAvailable{{JobMessageBase: actionsscale.JobMessageBase{RunnerRequestID: 202}}},
		},
	)

	first, err := client.Statistics(context.Background(), identity, 0)
	if err != nil || first.TotalAssignedJobs != 0 {
		t.Fatalf("unassigned cancellation poll = %#v, error = %v", first, err)
	}
	if len(events.completions) != 0 {
		t.Fatalf("unassigned completion was written to runner index: %#v", events.completions)
	}
	if len(observer.completions) != 1 || observer.completions[0].poolID != "org" || observer.completions[0].result != "Canceled" || observer.completions[0].assigned {
		t.Fatalf("unassigned cancellation observation = %#v", observer.completions)
	}
	second, err := client.Statistics(context.Background(), identity, 1)
	if err != nil || second.TotalAssignedJobs != 1 {
		t.Fatalf("capacity recovery poll = %#v, error = %v", second, err)
	}
	if got := fmt.Sprint(api.session.callOrder()); got != "[get delete get delete acquire]" {
		t.Fatalf("message order = %s", got)
	}
	if got := fmt.Sprint(api.session.lastMessageIDs()); got != "[0 20]" {
		t.Fatalf("listener cursors = %s, want acknowledged cancellation cursor", got)
	}
	if got := fmt.Sprint(api.session.acquiredRequestIDs()); got != "[202]" {
		t.Fatalf("acquired runner requests = %s", got)
	}
}

func TestOfficialIdentifiedCompletionIsPersistedExactly(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
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
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID: 30, Statistics: &actionsscale.RunnerScaleSetStatistic{},
		JobCompletedMessages: []*actionsscale.JobCompleted{{
			RunnerID: 41, RunnerName: "runner-1", Result: "Succeeded",
			JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"},
		}},
	})
	if _, err := client.Statistics(context.Background(), identity, 0); err != nil {
		t.Fatal(err)
	}
	want := recordedCompletion{poolID: "org", runnerName: "runner-1", jobID: "job-1", result: "Succeeded"}
	if len(events.completions) != 1 || events.completions[0] != want {
		t.Fatalf("persisted completions = %#v, want %#v", events.completions, want)
	}
}

func TestOfficialMalformedJobCompletionFailsBeforeAcknowledgement(t *testing.T) {
	t.Parallel()
	tests := map[string]*actionsscale.JobCompleted{
		"nil":                    nil,
		"missing-job-id":         {RunnerID: 41, RunnerName: "runner-1", Result: "Succeeded"},
		"missing-result":         {RunnerID: 41, RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}},
		"runner-name-only":       {RunnerName: "runner-1", Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}},
		"runner-id-only":         {RunnerID: 41, Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}},
		"anonymous-success":      {Result: "Succeeded", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}},
		"anonymous-assigned":     {Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1", RunnerAssignTime: time.Unix(1, 0).UTC()}},
		"whitespace-runner-name": {RunnerID: 41, RunnerName: " ", Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}},
	}
	for name, completed := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeOfficialAPI()
			client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
			identity, err := client.Ensure(context.Background(), testDefinition(), nil)
			if err != nil {
				t.Fatal(err)
			}
			api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
				MessageID: 40, Statistics: &actionsscale.RunnerScaleSetStatistic{},
				JobCompletedMessages: []*actionsscale.JobCompleted{completed},
			})
			_, err = client.Statistics(context.Background(), identity, 0)
			if !IsKind(err, ErrorInvalid) {
				t.Fatalf("error = %v, want invalid lifecycle event", err)
			}
			if got := fmt.Sprint(api.session.callOrder()); got != "[get]" {
				t.Fatalf("malformed completion was acknowledged: %s", got)
			}
		})
	}
}

func TestOfficialAnonymousCompletionConflictingWithBatchStartFailsBeforePersistence(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
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
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID: 50, Statistics: &actionsscale.RunnerScaleSetStatistic{},
		JobStartedMessages: []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
		JobCompletedMessages: []*actionsscale.JobCompleted{{
			Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"},
		}},
	})
	_, err = client.Statistics(context.Background(), identity, 0)
	if !IsKind(err, ErrorInvalid) {
		t.Fatalf("error = %v, want conflicting anonymous completion rejection", err)
	}
	if events.started != 0 || len(events.completions) != 0 {
		t.Fatalf("invalid batch partially persisted: starts=%d completions=%#v", events.started, events.completions)
	}
	if got := fmt.Sprint(api.session.callOrder()); got != "[get]" {
		t.Fatalf("invalid batch was acknowledged: %s", got)
	}
}

func TestOfficialIdentifiedStartAndCompletionInSameBatchConverge(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
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
	api.session.messages = append(api.session.messages, &actionsscale.RunnerScaleSetMessage{
		MessageID: 51, Statistics: &actionsscale.RunnerScaleSetStatistic{},
		JobStartedMessages: []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
		JobCompletedMessages: []*actionsscale.JobCompleted{{
			RunnerID: 41, RunnerName: "runner-1", Result: "Canceled",
			JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1", RunnerAssignTime: time.Unix(1, 0).UTC()},
		}},
	})
	if _, err := client.Statistics(context.Background(), identity, 0); err != nil {
		t.Fatal(err)
	}
	if events.started != 1 || len(events.completions) != 1 {
		t.Fatalf("identified batch persistence = starts:%d completions:%#v", events.started, events.completions)
	}
	if _, active := client.ActiveJob("org", "runner-1"); active {
		t.Fatal("identified same-batch completion left the runner job active")
	}
	if got := fmt.Sprint(api.session.callOrder()); got != "[get delete]" {
		t.Fatalf("identified batch order = %s", got)
	}
}

func TestOfficialAnonymousCompletionCannotClearPreviouslyActiveJob(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	identity, err := client.Ensure(context.Background(), testDefinition(), nil)
	if err != nil {
		t.Fatal(err)
	}
	api.session.messages = append(api.session.messages,
		&actionsscale.RunnerScaleSetMessage{
			MessageID: 52, Statistics: &actionsscale.RunnerScaleSetStatistic{TotalAssignedJobs: 1},
			JobStartedMessages: []*actionsscale.JobStarted{{RunnerName: "runner-1", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
		},
		&actionsscale.RunnerScaleSetMessage{
			MessageID: 53, Statistics: &actionsscale.RunnerScaleSetStatistic{},
			JobCompletedMessages: []*actionsscale.JobCompleted{{Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-1"}}},
		},
	)
	if _, err := client.Statistics(context.Background(), identity, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Statistics(context.Background(), identity, 0); !IsKind(err, ErrorInvalid) {
		t.Fatalf("error = %v, want active-job conflict", err)
	}
	if jobID, active := client.ActiveJob("org", "runner-1"); !active || jobID != "job-1" {
		t.Fatalf("active job was lost: %q %t", jobID, active)
	}
	if got := fmt.Sprint(api.session.callOrder()); got != "[get delete get]" {
		t.Fatalf("active conflict order = %s", got)
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
	observer := &recordingJobEventObserver{}
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Events: events, Observer: observer,
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
		JobCompletedMessages: []*actionsscale.JobCompleted{
			{RunnerID: 42, RunnerName: "runner-2", Result: "Succeeded", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-2"}},
			{Result: "Canceled", JobMessageBase: actionsscale.JobMessageBase{JobID: "job-3"}},
		},
	}
	api.session.messages = append(api.session.messages, message)
	if _, err := client.Statistics(context.Background(), identity, 1); !errors.Is(err, deleteErr) {
		t.Fatalf("delete error = %v", err)
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get delete]" {
		t.Fatalf("delete failure call order = %v", got)
	}
	if len(observer.starts) != 0 || len(observer.completions) != 0 {
		t.Fatalf("failed acknowledgement emitted additive telemetry: starts=%v completions=%#v", observer.starts, observer.completions)
	}
	api.session.deleteErr = nil
	api.session.messages = append(api.session.messages, message)
	if _, err := client.Statistics(context.Background(), identity, 1); err != nil {
		t.Fatal(err)
	}
	if events.started != 2 || len(events.completions) != 2 {
		t.Fatalf("redelivered lifecycle persistence calls = starts:%d completions:%d, want two idempotent upserts each", events.started, len(events.completions))
	}
	if len(observer.starts) != 1 || len(observer.completions) != 2 || !observer.completions[0].assigned || observer.completions[1].assigned || observer.completions[1].result != "Canceled" {
		t.Fatalf("redelivered lifecycle telemetry = starts:%v completions:%#v, want exactly one acknowledged batch", observer.starts, observer.completions)
	}
	if got := api.session.callOrder(); fmt.Sprint(got) != "[get delete get delete acquire]" {
		t.Fatalf("redelivery call order = %v", got)
	}
}

func TestOfficialAcquireFailureRetainsPersistedActiveJobState(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	api.session.acquireErr = errors.New("acquire failed after irreversible delete")
	observer := &recordingJobEventObserver{}
	client, err := NewOfficialClient(OfficialOptions{
		HostID: "melo-desk-001", Version: "test", RequestTimeout: time.Minute,
		Secrets: fakeSecretStore{}, Factory: &fakeOfficialFactory{api: api}, Observer: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if len(observer.starts) != 1 {
		t.Fatalf("acknowledged message lost telemetry after acquire failure: %v", observer.starts)
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

func TestOfficialRunnerRegistrationRequiresExactIdentityAndNormalizesNotFound(t *testing.T) {
	t.Parallel()
	api := newFakeOfficialAPI()
	client := newOfficialForTest(t, &fakeOfficialFactory{api: api})
	if _, err := client.Ensure(context.Background(), testDefinition(), nil); err != nil {
		t.Fatal(err)
	}
	// The pinned client documents RunnerScaleSetID as optional on this lookup;
	// the exact ID and name still prove the one-job registration identity.
	api.runner = &actionsscale.RunnerReference{ID: 99, Name: "runner-1"}
	registered, err := client.RunnerRegistered(context.Background(), "org", 99, "runner-1")
	if err != nil || !registered {
		t.Fatalf("registered = %t, error = %v", registered, err)
	}

	api.runnerErr = fmt.Errorf("get runner: %w", actionsscale.RunnerNotFoundError)
	registered, err = client.RunnerRegistered(context.Background(), "org", 99, "runner-1")
	if err != nil || registered {
		t.Fatalf("missing registered = %t, error = %v", registered, err)
	}

	api.runnerErr = fmt.Errorf(`request failed(status="404 Not Found")`)
	registered, err = client.RunnerRegistered(context.Background(), "org", 99, "runner-1")
	if registered || !IsKind(err, ErrorNotFound) {
		t.Fatalf("generic 404 registered = %t, error = %v; want fail-closed typed error", registered, err)
	}

	api.runnerErr = nil
	api.runner = &actionsscale.RunnerReference{ID: 99, Name: "different-runner", RunnerScaleSetID: 42}
	registered, err = client.RunnerRegistered(context.Background(), "org", 99, "runner-1")
	if registered || !IsKind(err, ErrorInvalid) {
		t.Fatalf("mismatched registered = %t, error = %v", registered, err)
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

func TestOfficialErrorPassesContextCancellationUnwrapped(t *testing.T) {
	t.Parallel()
	// A canceled listener long poll is the controller's own designed
	// supersession, not a GitHub failure. It must stay context.Canceled and
	// never become a classified *Error, so the controller can recognize it as
	// benign rather than recording a spurious scale-set poll failure.
	for name, err := range map[string]error{
		"bare":    context.Canceled,
		"wrapped": fmt.Errorf("failed to get next message: %w", context.Canceled),
	} {
		got := translateOfficialError("poll", err)
		if !errors.Is(got, context.Canceled) {
			t.Errorf("%s: context cancellation not passed through: %#v", name, got)
		}
		var typed *Error
		if errors.As(got, &typed) {
			t.Errorf("%s: context cancellation classified as *Error: %#v", name, typed)
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

type recordedCompletion struct {
	poolID, runnerName, jobID, result string
}

type recordingJobEventSink struct {
	started     int
	completions []recordedCompletion
}

type observedJobCompletion struct {
	poolID   string
	result   string
	assigned bool
}

type recordingJobEventObserver struct {
	completions []observedJobCompletion
	starts      []time.Duration
}

func (o *recordingJobEventObserver) ObserveJobStarted(_ context.Context, _ string, visibilityLag time.Duration) {
	o.starts = append(o.starts, visibilityLag)
}
func (o *recordingJobEventObserver) ObserveJobCompleted(_ context.Context, poolID, result string, assigned bool) {
	o.completions = append(o.completions, observedJobCompletion{poolID: poolID, result: result, assigned: assigned})
}

func (s *recordingJobEventSink) JobStarted(context.Context, string, string, string) error {
	s.started++
	return nil
}
func (s *recordingJobEventSink) JobCompleted(_ context.Context, poolID, runnerName, jobID, result string) error {
	s.completions = append(s.completions, recordedCompletion{poolID: poolID, runnerName: runnerName, jobID: jobID, result: result})
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
	runner         *actionsscale.RunnerReference
	runnerErr      error
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
func (a *fakeOfficialAPI) GetRunner(_ context.Context, _ int) (*actionsscale.RunnerReference, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runnerErr != nil {
		return nil, a.runnerErr
	}
	if a.runner == nil {
		return nil, nil
	}
	copy := *a.runner
	return &copy, nil
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
	lastIDs       []int
	acquiredIDs   []int64
	getMessageErr error
	deleteErr     error
	acquireErr    error
}

func (s *fakeOfficialSession) Session() actionsscale.RunnerScaleSetSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
}
func (s *fakeOfficialSession) GetMessage(_ context.Context, lastID int, max int) (*actionsscale.RunnerScaleSetMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "get")
	s.lastIDs = append(s.lastIDs, lastID)
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
	s.acquiredIDs = append(s.acquiredIDs, ids...)
	return append([]int64(nil), ids...), s.acquireErr
}
func (*fakeOfficialSession) Close(context.Context) error { return nil }
func (s *fakeOfficialSession) callOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}
func (s *fakeOfficialSession) lastMessageIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.lastIDs...)
}
func (s *fakeOfficialSession) acquiredRequestIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.acquiredIDs...)
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
