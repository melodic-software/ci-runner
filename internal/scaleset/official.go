package scaleset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	actionsscale "github.com/actions/scaleset"
	"github.com/hashicorp/go-retryablehttp"
)

const (
	OfficialClientVersion = "v0.4.0"
	OfficialClientCommit  = "6ce025902cd964747a078c2aabe7340ebc667eca"
)

type JobEventSink interface {
	JobStarted(context.Context, string, string, string) error
	// JobCompleted receives only completions with an exact runner identity.
	// Valid canceled-before-assignment events are not runner-indexable and are
	// acknowledged without calling this method.
	JobCompleted(context.Context, string, string, string, string) error
}

type DiscardJobEventSink struct{}

func (DiscardJobEventSink) JobStarted(context.Context, string, string, string) error { return nil }
func (DiscardJobEventSink) JobCompleted(context.Context, string, string, string, string) error {
	return nil
}

// OfficialOptions contains only controller identity and transport policy.
// Credentials are fetched by SecretID from SecretStore and remain in memory.
type OfficialOptions struct {
	HostID         string
	Version        string
	CommitSHA      string
	RequestTimeout time.Duration
	Secrets        SecretStore
	Events         JobEventSink
	Factory        officialFactory
}

type OfficialClient struct {
	opts OfficialOptions

	mu         sync.RWMutex
	targets    map[string]*officialTarget
	byScaleSet map[int64]*officialTarget
	activeJobs map[string]map[string]string
}

type officialTarget struct {
	mu sync.Mutex

	definition Definition
	api        officialAPI
	scaleSet   *actionsscale.RunnerScaleSet
	session    officialSession
	lastID     int
	statistics actionsscale.RunnerScaleSetStatistic
}

func NewOfficialClient(options OfficialOptions) (*OfficialClient, error) {
	if options.HostID == "" || options.Version == "" || options.RequestTimeout <= 0 || options.Secrets == nil {
		return nil, errors.New("official scale-set client requires host ID, version, request timeout, and secret store")
	}
	if options.Events == nil {
		options.Events = DiscardJobEventSink{}
	}
	if options.Factory == nil {
		options.Factory = &defaultOfficialFactory{options: options}
	}
	return &OfficialClient{
		opts: options, targets: map[string]*officialTarget{}, byScaleSet: map[int64]*officialTarget{},
		activeJobs: map[string]map[string]string{},
	}, nil
}

func (c *OfficialClient) Ensure(ctx context.Context, definition Definition, previous *Identity) (Identity, error) {
	if err := validateDefinition(definition); err != nil {
		return Identity{}, &Error{Kind: ErrorInvalid, Operation: "ensure", Err: err}
	}
	target, err := c.targetClient(ctx, definition)
	if err != nil {
		return Identity{}, translateOfficialError("create client", err)
	}
	target.mu.Lock()
	defer target.mu.Unlock()

	groupID, err := runnerGroupID(ctx, target.api, definition.RunnerGroup)
	if err != nil {
		return Identity{}, translateOfficialError("get runner group", err)
	}
	var current *actionsscale.RunnerScaleSet
	if previous != nil && previous.ScaleSetID > 0 {
		current, err = target.api.GetRunnerScaleSetByID(ctx, int(previous.ScaleSetID))
		if err != nil {
			// A lookup by the configured immutable identity distinguishes a deleted
			// scale set from a general authentication/transport failure without
			// creating a duplicate.
			current, err = target.api.GetRunnerScaleSet(ctx, groupID, definition.ScaleSetName)
		}
	} else {
		current, err = target.api.GetRunnerScaleSet(ctx, groupID, definition.ScaleSetName)
	}
	if err != nil {
		return Identity{}, translateOfficialError("get runner scale set", err)
	}
	desiredLabels := buildLabels(definition)
	if current == nil {
		current, err = target.api.CreateRunnerScaleSet(ctx, &actionsscale.RunnerScaleSet{
			Name: definition.ScaleSetName, RunnerGroupID: groupID, Labels: desiredLabels,
			RunnerSetting: actionsscale.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return Identity{}, translateOfficialError("create runner scale set", err)
		}
	} else {
		if current.Name != definition.ScaleSetName || current.RunnerGroupID != groupID {
			return Identity{}, &Error{Kind: ErrorConflict, Operation: "ensure", Err: errors.New("persisted scale set does not match configured name and runner group")}
		}
		if !current.RunnerSetting.DisableUpdate || !sameLabels(current.Labels, desiredLabels) {
			updated := *current
			updated.Labels = desiredLabels
			updated.RunnerSetting.DisableUpdate = true
			current, err = target.api.UpdateRunnerScaleSet(ctx, current.ID, &updated)
			if err != nil {
				return Identity{}, translateOfficialError("update runner scale set", err)
			}
		}
	}
	if current == nil || current.ID <= 0 {
		return Identity{}, &Error{Kind: ErrorInvalid, Operation: "ensure", Err: errors.New("GitHub returned an empty scale-set identity")}
	}

	forceNewSession := previous == nil && target.session != nil
	if target.session == nil || target.scaleSet == nil || target.scaleSet.ID != current.ID || forceNewSession {
		if target.session != nil {
			_ = target.session.Close(context.WithoutCancel(ctx))
		}
		session, sessionErr := target.api.MessageSession(ctx, current.ID, c.opts.HostID+"-"+definition.TargetID)
		if sessionErr != nil {
			return Identity{}, translateOfficialError("create message session", sessionErr)
		}
		initial := session.Session()
		if initial.Statistics == nil || initial.SessionID.String() == "00000000-0000-0000-0000-000000000000" {
			_ = session.Close(context.WithoutCancel(ctx))
			return Identity{}, &Error{Kind: ErrorInvalid, Operation: "create message session", Err: errors.New("GitHub returned an invalid initial session")}
		}
		target.session = session
		target.lastID = 0
		target.statistics = *initial.Statistics
	}
	target.definition = definition
	target.scaleSet = current

	c.mu.Lock()
	for id, existing := range c.byScaleSet {
		if existing == target && id != int64(current.ID) {
			delete(c.byScaleSet, id)
		}
	}
	c.byScaleSet[int64(current.ID)] = target
	c.mu.Unlock()
	return Identity{ScaleSetID: int64(current.ID), ListenerID: target.session.Session().SessionID.String()}, nil
}

func (c *OfficialClient) Statistics(ctx context.Context, identity Identity, maxCapacity int) (Statistics, error) {
	if maxCapacity < 0 {
		return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "poll", Err: errors.New("max capacity must not be negative")}
	}
	c.mu.RLock()
	target := c.byScaleSet[identity.ScaleSetID]
	c.mu.RUnlock()
	if target == nil {
		return Statistics{}, &Error{Kind: ErrorNotFound, Operation: "poll", Err: errors.New("scale-set session is not initialized")}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.session == nil || target.session.Session().SessionID.String() != identity.ListenerID {
		return Statistics{}, &Error{Kind: ErrorNotFound, Operation: "poll", Err: errors.New("message-session identity changed")}
	}
	message, err := target.session.GetMessage(ctx, target.lastID, maxCapacity)
	if err != nil {
		return Statistics{}, translateOfficialError("poll", err)
	}
	if message == nil {
		return Statistics{TotalAssignedJobs: target.statistics.TotalAssignedJobs}, nil
	}
	if message.Statistics == nil {
		return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "poll", Err: errors.New("message did not contain authoritative statistics")}
	}
	// Validate the whole lifecycle batch before the first durable write. A
	// malformed completion must not leave a newly-started job active forever
	// merely because its start happened earlier in this same message.
	startedJobIDs := make(map[string]struct{}, len(message.JobStartedMessages))
	for _, started := range message.JobStartedMessages {
		if started == nil || strings.TrimSpace(started.JobID) == "" {
			return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "validate job-started event", Err: errors.New("job-started event is nil or missing its job ID")}
		}
		startedJobIDs[strings.TrimSpace(started.JobID)] = struct{}{}
	}
	identifiedCompletions := make([]*actionsscale.JobCompleted, 0, len(message.JobCompletedMessages))
	for _, completed := range message.JobCompletedMessages {
		identified, validationErr := validateJobCompletion(completed)
		if validationErr != nil {
			return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "validate job-completed event", Err: validationErr}
		}
		if !identified {
			jobID := strings.TrimSpace(completed.JobID)
			if _, startedInBatch := startedJobIDs[jobID]; startedInBatch {
				return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "validate job-completed event", Err: errors.New("anonymous completion conflicts with a job-started event in the same batch")}
			}
			if _, alreadyActive := c.activeRunnerForJob(target.definition.TargetID, jobID); alreadyActive {
				return Statistics{}, &Error{Kind: ErrorInvalid, Operation: "validate job-completed event", Err: errors.New("anonymous completion conflicts with an active runner job")}
			}
			// GitHub emits a terminal completion with no runner identity when a
			// job is canceled before assignment. It has no runner-index state to
			// persist or clear, but is still a valid lifecycle event that must not
			// poison message acknowledgement and later capacity updates.
			continue
		}
		identifiedCompletions = append(identifiedCompletions, completed)
	}
	// Persist lifecycle identity before either acquiring or acknowledging the
	// message. Redelivery is expected, so the sink contract is idempotent.
	for _, started := range message.JobStartedMessages {
		if err := c.opts.Events.JobStarted(ctx, target.definition.TargetID, started.RunnerName, started.JobID); err != nil {
			return Statistics{}, &Error{Kind: ErrorTransport, Operation: "persist job-started event", Err: err}
		}
	}
	for _, completed := range identifiedCompletions {
		if err := c.opts.Events.JobCompleted(ctx, target.definition.TargetID, completed.RunnerName, completed.JobID, completed.Result); err != nil {
			return Statistics{}, &Error{Kind: ErrorTransport, Operation: "persist job-completed event", Err: err}
		}
	}
	// Update the in-memory acceleration map immediately after the durable sink.
	// DeleteMessage is irreversible: if a later AcquireJobs call fails, this
	// message will not replay and delaying these updates would forget lifecycle
	// state until process restart. Redelivery remains idempotent.
	for _, started := range message.JobStartedMessages {
		c.setActiveJob(target.definition.TargetID, started.RunnerName, started.JobID)
	}
	for _, completed := range identifiedCompletions {
		c.clearActiveJob(target.definition.TargetID, completed.RunnerName)
	}
	// Preserve the pinned official listener's acknowledge-before-acquire order.
	// Lifecycle evidence above is persisted first to close the crash gap; if the
	// acknowledgement fails, no acquisition occurs and redelivery is idempotent.
	if err := target.session.DeleteMessage(context.WithoutCancel(ctx), message.MessageID); err != nil {
		return Statistics{}, translateOfficialError("acknowledge message", err)
	}
	if len(message.JobAvailableMessages) > 0 {
		requestIDs := make([]int64, 0, len(message.JobAvailableMessages))
		for _, available := range message.JobAvailableMessages {
			requestIDs = append(requestIDs, available.RunnerRequestID)
		}
		if _, err := target.session.AcquireJobs(context.WithoutCancel(ctx), requestIDs); err != nil {
			return Statistics{}, translateOfficialError("acquire jobs", err)
		}
	}
	target.lastID = message.MessageID
	target.statistics = *message.Statistics
	return Statistics{TotalAssignedJobs: target.statistics.TotalAssignedJobs}, nil
}

// validateJobCompletion returns true only when the completion has an exact
// runner identity that belongs in the durable runner/job index. GitHub's
// canceled-before-assignment event deliberately has neither identity field;
// accepting that shape is restricted to a real job ID, cancellation result,
// and the absence of a runner-assignment timestamp.
func validateJobCompletion(completed *actionsscale.JobCompleted) (bool, error) {
	if completed == nil {
		return false, errors.New("job-completed event is nil")
	}
	if strings.TrimSpace(completed.JobID) == "" {
		return false, errors.New("job-completed event is missing its job ID")
	}
	result := strings.TrimSpace(completed.Result)
	if result == "" {
		return false, errors.New("job-completed event is missing its result")
	}
	if completed.RunnerID == 0 && completed.RunnerName == "" {
		if !completed.RunnerAssignTime.IsZero() {
			return false, errors.New("job-completed event without a runner identity has a runner assignment time")
		}
		if !strings.EqualFold(result, "canceled") && !strings.EqualFold(result, "cancelled") {
			return false, errors.New("job-completed event without a runner identity must be canceled")
		}
		return false, nil
	}
	if completed.RunnerID <= 0 || strings.TrimSpace(completed.RunnerName) == "" {
		return false, errors.New("job-completed event has a partial runner identity")
	}
	return true, nil
}

func (c *OfficialClient) CreateJITConfig(ctx context.Context, identity Identity, runnerName string) (JITConfig, error) {
	if runnerName == "" {
		return JITConfig{}, &Error{Kind: ErrorInvalid, Operation: "create JIT configuration", Err: errors.New("runner name is required")}
	}
	c.mu.RLock()
	target := c.byScaleSet[identity.ScaleSetID]
	c.mu.RUnlock()
	if target == nil {
		return JITConfig{}, &Error{Kind: ErrorNotFound, Operation: "create JIT configuration", Err: errors.New("scale set is not initialized")}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	result, err := target.api.GenerateJITConfig(ctx, &actionsscale.RunnerScaleSetJitRunnerSetting{Name: runnerName}, int(identity.ScaleSetID))
	if err != nil {
		return JITConfig{}, translateOfficialError("create JIT configuration", err)
	}
	if result == nil || result.EncodedJITConfig == "" || result.Runner == nil || result.Runner.ID <= 0 || result.Runner.Name != runnerName {
		return JITConfig{}, &Error{Kind: ErrorInvalid, Operation: "create JIT configuration", Err: errors.New("GitHub returned an invalid JIT runner response")}
	}
	return NewRunnerJITConfig([]byte(result.EncodedJITConfig), int64(result.Runner.ID)), nil
}

func (c *OfficialClient) RunnerRegistered(ctx context.Context, poolID string, runnerID int64, runnerName string) (bool, error) {
	if poolID == "" || runnerID <= 0 || runnerName == "" {
		return false, &Error{Kind: ErrorInvalid, Operation: "get runner", Err: errors.New("pool ID, positive runner ID, and runner name are required")}
	}
	c.mu.RLock()
	target := c.targets[poolID]
	c.mu.RUnlock()
	if target == nil {
		return false, &Error{Kind: ErrorNotFound, Operation: "get runner", Err: errors.New("scale-set target is not initialized")}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if target.scaleSet == nil || target.scaleSet.ID <= 0 {
		return false, &Error{Kind: ErrorNotFound, Operation: "get runner", Err: errors.New("scale set is not initialized")}
	}
	runner, err := target.api.GetRunner(ctx, int(runnerID))
	if err != nil {
		// Only the Scale Set Client's parsed AgentNotFoundException proves
		// this exact registration is gone. A generic HTTP 404 can come from
		// token refresh, admin-connection discovery, or a wrong scope and must
		// remain an error so cleanup fails closed.
		if errors.Is(err, actionsscale.RunnerNotFoundError) {
			return false, nil
		}
		return false, translateOfficialError("get runner", err)
	}
	if runner == nil || int64(runner.ID) != runnerID || runner.Name != runnerName || runner.RunnerScaleSetID != 0 && runner.RunnerScaleSetID != target.scaleSet.ID {
		return false, &Error{Kind: ErrorInvalid, Operation: "get runner", Err: errors.New("GitHub returned a mismatched runner identity")}
	}
	return true, nil
}

func (c *OfficialClient) RemoveRunner(ctx context.Context, poolID string, runnerID int64) error {
	if poolID == "" || runnerID <= 0 {
		return &Error{Kind: ErrorInvalid, Operation: "remove runner", Err: errors.New("pool ID and positive runner ID are required")}
	}
	c.mu.RLock()
	target := c.targets[poolID]
	c.mu.RUnlock()
	if target == nil {
		return &Error{Kind: ErrorNotFound, Operation: "remove runner", Err: errors.New("scale-set target is not initialized")}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if err := target.api.RemoveRunner(ctx, runnerID); err != nil {
		return translateOfficialError("remove runner", err)
	}
	return nil
}

func (c *OfficialClient) ActiveJob(poolID, runnerName string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	jobID, ok := c.activeJobs[poolID][runnerName]
	return jobID, ok
}

// Close deletes message sessions only. It deliberately preserves scale sets;
// permanent uninstall must call DeleteScaleSet explicitly.
func (c *OfficialClient) Close(ctx context.Context) error {
	c.mu.RLock()
	targets := make([]*officialTarget, 0, len(c.targets))
	for _, target := range c.targets {
		targets = append(targets, target)
	}
	c.mu.RUnlock()
	var errs []error
	for _, target := range targets {
		target.mu.Lock()
		if target.session != nil {
			errs = append(errs, target.session.Close(ctx))
			target.session = nil
		}
		target.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (c *OfficialClient) DeleteScaleSet(ctx context.Context, scaleSetID int64) error {
	c.mu.RLock()
	target := c.byScaleSet[scaleSetID]
	c.mu.RUnlock()
	if target == nil {
		return &Error{Kind: ErrorNotFound, Operation: "uninstall", Err: errors.New("scale set is not initialized")}
	}
	target.mu.Lock()
	defer target.mu.Unlock()
	if err := target.api.DeleteRunnerScaleSet(ctx, int(scaleSetID)); err != nil {
		return translateOfficialError("uninstall", err)
	}
	return nil
}

func (c *OfficialClient) targetClient(ctx context.Context, definition Definition) (*officialTarget, error) {
	c.mu.RLock()
	target := c.targets[definition.TargetID]
	c.mu.RUnlock()
	if target != nil {
		if target.definition.URL != definition.URL || target.definition.ClientID != definition.ClientID || target.definition.InstallationID != definition.InstallationID || target.definition.SecretID != definition.SecretID {
			return nil, errors.New("immutable target authentication fields changed while controller is running")
		}
		return target, nil
	}
	key, err := c.opts.Secrets.PrivateKey(ctx, definition.SecretID)
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}
	api, err := c.opts.Factory.New(ctx, definition, key)
	if err != nil {
		return nil, err
	}
	target = &officialTarget{definition: definition, api: api}
	c.mu.Lock()
	if existing := c.targets[definition.TargetID]; existing != nil {
		c.mu.Unlock()
		return existing, nil
	}
	c.targets[definition.TargetID] = target
	c.mu.Unlock()
	return target, nil
}

func (c *OfficialClient) setActiveJob(poolID, runnerName, jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeJobs[poolID] == nil {
		c.activeJobs[poolID] = map[string]string{}
	}
	c.activeJobs[poolID][runnerName] = jobID
}

func (c *OfficialClient) clearActiveJob(poolID, runnerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activeJobs[poolID], runnerName)
}

func (c *OfficialClient) activeRunnerForJob(poolID, jobID string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for runnerName, activeJobID := range c.activeJobs[poolID] {
		if activeJobID == jobID {
			return runnerName, true
		}
	}
	return "", false
}

func validateDefinition(definition Definition) error {
	if definition.TargetID == "" || definition.URL == "" || definition.ClientID == "" || definition.InstallationID <= 0 || definition.SecretID == "" || definition.ScaleSetName == "" {
		return errors.New("target ID, URL, client ID, installation ID, secret ID, and scale-set name are required")
	}
	return nil
}

func runnerGroupID(ctx context.Context, api officialAPI, name string) (int, error) {
	if name == "" || name == actionsscale.DefaultRunnerGroup {
		return 1, nil
	}
	group, err := api.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, err
	}
	if group == nil || group.ID <= 0 {
		return 0, errors.New("GitHub returned an invalid runner group")
	}
	return group.ID, nil
}

func buildLabels(definition Definition) []actionsscale.Label {
	labels := definition.Labels
	if len(labels) == 0 {
		labels = []string{definition.ScaleSetName}
	}
	result := make([]actionsscale.Label, 0, len(labels))
	for _, label := range labels {
		result = append(result, actionsscale.Label{Name: label, Type: "System"})
	}
	return result
}

func sameLabels(a, b []actionsscale.Label) bool {
	left := make([]string, 0, len(a))
	right := make([]string, 0, len(b))
	for _, label := range a {
		left = append(left, label.Type+"\x00"+label.Name)
	}
	for _, label := range b {
		right = append(right, label.Type+"\x00"+label.Name)
	}
	sort.Strings(left)
	sort.Strings(right)
	return strings.Join(left, "\x01") == strings.Join(right, "\x01")
}

var officialStatusPattern = regexp.MustCompile(`status="([0-9]{3})`)

type officialRateLimitError struct {
	retryAfterSeconds int
	err               error
}

func (e *officialRateLimitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return "official scale-set client was rate limited"
}

func (e *officialRateLimitError) Unwrap() error { return e.err }

func translateOfficialError(operation string, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// The official client has its own configured request deadline. The outer
		// retry loop's context check prevents retrying a caller deadline, while an
		// internal HTTP deadline is a safe transport retry.
		return &Error{Kind: ErrorTransport, Operation: operation, Err: err}
	}
	var rateLimit *officialRateLimitError
	if errors.As(err, &rateLimit) {
		return &Error{
			Kind: ErrorRateLimited, Operation: operation, StatusCode: http.StatusTooManyRequests,
			RetryAfterSeconds: rateLimit.retryAfterSeconds, Err: err,
		}
	}
	if errors.Is(err, actionsscale.RunnerNotFoundError) {
		return &Error{Kind: ErrorNotFound, Operation: operation, Err: err}
	}
	kind := ErrorTransport
	status := 0
	if match := officialStatusPattern.FindStringSubmatch(err.Error()); len(match) == 2 {
		status, _ = strconv.Atoi(match[1])
		switch status {
		case http.StatusUnauthorized:
			kind = ErrorUnauthorized
		case http.StatusForbidden:
			kind = ErrorForbidden
		case http.StatusNotFound:
			kind = ErrorNotFound
		case http.StatusConflict:
			kind = ErrorConflict
		case http.StatusTooManyRequests:
			kind = ErrorRateLimited
		default:
			if status >= 500 {
				kind = ErrorServer
			} else {
				kind = ErrorInvalid
			}
		}
	}
	return &Error{Kind: kind, Operation: operation, StatusCode: status, Err: err}
}

type officialFactory interface {
	New(context.Context, Definition, SecretMaterial) (officialAPI, error)
}

type officialAPI interface {
	GetRunnerGroupByName(context.Context, string) (*actionsscale.RunnerGroup, error)
	GetRunnerScaleSet(context.Context, int, string) (*actionsscale.RunnerScaleSet, error)
	GetRunnerScaleSetByID(context.Context, int) (*actionsscale.RunnerScaleSet, error)
	CreateRunnerScaleSet(context.Context, *actionsscale.RunnerScaleSet) (*actionsscale.RunnerScaleSet, error)
	UpdateRunnerScaleSet(context.Context, int, *actionsscale.RunnerScaleSet) (*actionsscale.RunnerScaleSet, error)
	DeleteRunnerScaleSet(context.Context, int) error
	MessageSession(context.Context, int, string) (officialSession, error)
	GenerateJITConfig(context.Context, *actionsscale.RunnerScaleSetJitRunnerSetting, int) (*actionsscale.RunnerScaleSetJitRunnerConfig, error)
	GetRunner(context.Context, int) (*actionsscale.RunnerReference, error)
	RemoveRunner(context.Context, int64) error
}

type officialSession interface {
	Session() actionsscale.RunnerScaleSetSession
	GetMessage(context.Context, int, int) (*actionsscale.RunnerScaleSetMessage, error)
	DeleteMessage(context.Context, int) error
	AcquireJobs(context.Context, []int64) ([]int64, error)
	Close(context.Context) error
}

type defaultOfficialFactory struct{ options OfficialOptions }

func (f *defaultOfficialFactory) New(_ context.Context, definition Definition, key SecretMaterial) (officialAPI, error) {
	privateKey := key.Reveal()
	defer clear(privateKey)
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 0
	retryClient.Logger = nil
	retryClient.ErrorHandler = officialRetryErrorHandler
	client, err := actionsscale.NewClientWithGitHubApp(actionsscale.ClientWithGitHubAppConfig{
		GitHubConfigURL: definition.URL,
		GitHubAppAuth: actionsscale.GitHubAppAuth{
			ClientID: definition.ClientID, InstallationID: definition.InstallationID, PrivateKey: string(privateKey),
		},
		SystemInfo: actionsscale.SystemInfo{
			System: "ci-runner", Version: f.options.Version, CommitSHA: f.options.CommitSHA, Subsystem: "controller",
		},
	}, actionsscale.WithTimeout(f.options.RequestTimeout), actionsscale.WithRetryableHTTPClint(retryClient), actionsscale.WithRetryMax(0))
	if err != nil {
		return nil, err
	}
	return &officialAPIWrapper{Client: client}, nil
}

func officialRetryErrorHandler(response *http.Response, err error, _ int) (*http.Response, error) {
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		return response, err
	}
	retryAfter := 0
	if value := strings.TrimSpace(response.Header.Get("Retry-After")); value != "" {
		if parsed, parseErr := strconv.Atoi(value); parseErr == nil && parsed > 0 {
			retryAfter = parsed
		}
	}
	return response, &officialRateLimitError{retryAfterSeconds: retryAfter, err: err}
}

type officialAPIWrapper struct{ *actionsscale.Client }

func (c *officialAPIWrapper) MessageSession(ctx context.Context, scaleSetID int, owner string) (officialSession, error) {
	return c.Client.MessageSessionClient(ctx, scaleSetID, owner, actionsscale.WithRetryMax(0))
}
func (c *officialAPIWrapper) GenerateJITConfig(ctx context.Context, setting *actionsscale.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*actionsscale.RunnerScaleSetJitRunnerConfig, error) {
	return c.Client.GenerateJitRunnerConfig(ctx, setting, scaleSetID)
}
func (c *officialAPIWrapper) GetRunner(ctx context.Context, runnerID int) (*actionsscale.RunnerReference, error) {
	return c.Client.GetRunner(ctx, runnerID)
}
func (c *officialAPIWrapper) RemoveRunner(ctx context.Context, runnerID int64) error {
	return c.Client.RemoveRunner(ctx, runnerID)
}

var _ Client = (*OfficialClient)(nil)
var _ JobLookup = (*OfficialClient)(nil)
