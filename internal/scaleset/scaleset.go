// Package scaleset defines the platform-neutral boundary to GitHub's official
// Runner Scale Set Client. The live adapter owns authentication and message
// sessions; controller policy sees only identities, aggregate statistics, and
// one-job JIT configurations.
package scaleset

import (
	"context"
	"errors"
	"fmt"
)

type Definition struct {
	TargetID       string
	URL            string
	Scope          string
	ClientID       string
	InstallationID int64
	SecretID       string
	RunnerGroup    string
	ScaleSetName   string
	Labels         []string
}

// Identity is persistent per physical host and target. Two controllers must
// never share either ScaleSetID or ListenerID.
type Identity struct {
	ScaleSetID int64
	ListenerID string
}

type Statistics struct {
	TotalAssignedJobs int
}

// SecretMaterial contains an in-memory PEM credential loaded from a native
// secret store. Formatting always redacts it and Reveal returns a copy.
type SecretMaterial struct{ value []byte }

func NewSecretMaterial(value []byte) SecretMaterial {
	return SecretMaterial{value: append([]byte(nil), value...)}
}
func (SecretMaterial) String() string   { return "[REDACTED SECRET]" }
func (SecretMaterial) GoString() string { return "[REDACTED SECRET]" }
func (s SecretMaterial) Reveal() []byte { return append([]byte(nil), s.value...) }
func (s SecretMaterial) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(s.String()))
}

type SecretStore interface {
	PrivateKey(context.Context, string) (SecretMaterial, error)
}

// JobLookup exposes acknowledged listener lifecycle events without coupling the
// controller to the official client's wire message types.
type JobLookup interface {
	ActiveJob(poolID, runnerName string) (string, bool)
}

// JITConfig intentionally has no exported secret-bearing field and redacts all
// common formatting paths. Reveal returns a copy for the WorkerRuntime boundary.
type JITConfig struct {
	encoded  []byte
	runnerID int64
}

func NewJITConfig(encoded []byte) JITConfig {
	return JITConfig{encoded: append([]byte(nil), encoded...)}
}

func NewRunnerJITConfig(encoded []byte, runnerID int64) JITConfig {
	return JITConfig{encoded: append([]byte(nil), encoded...), runnerID: runnerID}
}

func (JITConfig) String() string    { return "[REDACTED JIT CONFIG]" }
func (JITConfig) GoString() string  { return "[REDACTED JIT CONFIG]" }
func (j JITConfig) Reveal() []byte  { return append([]byte(nil), j.encoded...) }
func (j JITConfig) RunnerID() int64 { return j.runnerID }

type Client interface {
	// Ensure verifies or creates this host's scale set and message listener.
	// previous is nil on first boot and otherwise contains persisted identity.
	Ensure(context.Context, Definition, *Identity) (Identity, error)
	// Statistics reports maxCapacity on every listener poll and returns the
	// authoritative TotalAssignedJobs value from GitHub.
	Statistics(context.Context, Identity, int) (Statistics, error)
	// CreateJITConfig returns one ephemeral, one-job runner configuration.
	CreateJITConfig(context.Context, Identity, string) (JITConfig, error)
	// RemoveRunner deregisters an exact runner only after controller policy has
	// quiesced its pool and ruled out assigned/active work.
	RemoveRunner(context.Context, string, int64) error
}

type ErrorKind string

const (
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorForbidden    ErrorKind = "forbidden"
	ErrorNotFound     ErrorKind = "not-found"
	ErrorConflict     ErrorKind = "conflict"
	ErrorRateLimited  ErrorKind = "rate-limited"
	ErrorServer       ErrorKind = "server"
	ErrorTransport    ErrorKind = "transport"
	ErrorInvalid      ErrorKind = "invalid-response"
)

type Error struct {
	Kind              ErrorKind
	Operation         string
	StatusCode        int
	RetryAfterSeconds int
	Err               error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil {
		return fmt.Sprintf("scale set %s: %s: %v", e.Operation, e.Kind, e.Err)
	}
	return fmt.Sprintf("scale set %s: %s", e.Operation, e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

func IsKind(err error, kind ErrorKind) bool {
	var target *Error
	return errors.As(err, &target) && target.Kind == kind
}

func Retryable(err error) bool {
	var target *Error
	if !errors.As(err, &target) {
		return false
	}
	switch target.Kind {
	case ErrorConflict, ErrorRateLimited, ErrorServer, ErrorTransport:
		return true
	default:
		return false
	}
}
