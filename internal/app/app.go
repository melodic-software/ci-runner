// Package app implements the user-facing ci-runner command surface. It changes
// lifecycle intent only through desired state; the controller remains the sole
// owner of ordinary Docker and worker transitions.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/controller"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/secret"
	"github.com/melodic-software/ci-runner/internal/state"
)

const (
	ExitOK                = 0
	ExitUsage             = 2
	ExitInvalidConfig     = 3
	ExitCredential        = 4
	ExitDegraded          = 5
	ExitOperationTimedOut = 6
	ExitRuntime           = 7
	ExitStateChanged      = 8
)

type SecretImporter interface {
	Import(context.Context, string, string) (secret.ImportResult, error)
}

type ForceStopper interface {
	Preview(context.Context) ([]controller.ForceStopTarget, error)
	Execute(context.Context, []controller.ForceStopTarget) ([]controller.ForceStopTarget, error)
}

type ControllerControl interface {
	Status(context.Context) (control.Status, error)
	Shutdown(context.Context, string, control.Status, bool) (control.Status, error)
}

type RestartReceiptReader interface {
	LoadRestartReceipt(context.Context) (model.RestartReceipt, error)
}

type DoctorInspection struct {
	CheckDocker     bool
	RequireDocker   bool
	IncludeElevated bool
}

type DoctorInspector interface {
	Inspect(context.Context, DoctorInspection) []DoctorCheck
}

type Dependencies struct {
	Config          config.Config
	Store           state.Store
	Gaming          host.GamingHost
	Secrets         SecretImporter
	ForceStop       ForceStopper
	Logs            *FileLogs
	Control         ControllerControl
	Doctor          DoctorInspector
	Processes       host.ProcessObserver
	Tasks           host.ScheduledTaskStarter
	RestartReceipts RestartReceiptReader
	Now             func() time.Time
	PollInterval    time.Duration
}

type Application struct {
	dependencies Dependencies
	in           *bufio.Reader
	out          io.Writer
	errOut       io.Writer
}

func New(dependencies Dependencies, in io.Reader, out, errOut io.Writer) (*Application, error) {
	if dependencies.Store == nil {
		return nil, errors.New("state store is required")
	}
	if in == nil || out == nil || errOut == nil {
		return nil, errors.New("input and output streams are required")
	}
	if dependencies.Now == nil {
		dependencies.Now = func() time.Time { return time.Now().UTC() }
	}
	if dependencies.PollInterval <= 0 {
		dependencies.PollInterval = 500 * time.Millisecond
	}
	return &Application{
		dependencies: dependencies,
		in:           bufio.NewReader(in),
		out:          out,
		errOut:       errOut,
	}, nil
}

func (a *Application) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.usage()
		return ExitUsage
	}
	switch args[0] {
	case "host":
		return a.runHost(ctx, args[1:])
	case "secret":
		return a.runSecret(ctx, args[1:])
	case "help", "-h", "--help":
		a.usage()
		return ExitOK
	default:
		writef(a.errOut, "unknown command %q\n", args[0])
		a.usage()
		return ExitUsage
	}
}

func (a *Application) runHost(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.menu(ctx)
	}
	switch args[0] {
	case "status":
		return a.status(ctx, args[1:])
	case "enable":
		return a.enable(ctx, args[1:])
	case "disable":
		return a.disable(ctx, args[1:])
	case "game":
		return a.game(ctx, args[1:])
	case "doctor":
		return a.doctor(ctx, args[1:])
	case "logs":
		return a.logs(ctx, args[1:])
	case "force-stop":
		return a.forceStop(ctx, args[1:])
	case "controller":
		return a.controllerCommand(ctx, args[1:])
	default:
		writef(a.errOut, "unknown host command %q\n", args[0])
		return ExitUsage
	}
}

// writeIndentedJSON encodes value as pretty-printed JSON to a.out. On
// encoder failure it reports the error on a.errOut, naming the failed
// operation, and returns ExitRuntime; on success it returns ExitOK.
func (a *Application) writeIndentedJSON(value any, operation string) int {
	encoder := json.NewEncoder(a.out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		writef(a.errOut, "write %s: %v\n", operation, err)
		return ExitRuntime
	}
	return ExitOK
}

func (a *Application) status(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host status", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	jsonOutput := flags.Bool("json", false, "write machine-readable status")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	desired, desiredErr := a.dependencies.Store.LoadDesired(ctx)
	observed, observedErr := a.dependencies.Store.LoadObserved(ctx)
	controlHealthy := true
	if desiredErr != nil && !errors.Is(desiredErr, state.ErrNotFound) {
		writef(a.errOut, "read desired state: %v\n", desiredErr)
		return ExitRuntime
	}
	if observedErr != nil && !errors.Is(observedErr, state.ErrNotFound) {
		writef(a.errOut, "read observed state: %v\n", observedErr)
		return ExitRuntime
	}
	if *jsonOutput {
		value := statusDocument{SchemaVersion: 1, ActiveJobs: []activeJob{}}
		if desiredErr == nil {
			value.Desired = &desired
		}
		if observedErr == nil {
			value.Observed = &observed
			value.ControllerAvailable = a.dependencies.Control == nil
			for _, worker := range observed.Workers {
				if worker.State != model.WorkerBusy {
					continue
				}
				value.ActiveJobs = append(value.ActiveJobs, activeJob{
					WorkerID:  worker.ID,
					PoolID:    worker.PoolID,
					Name:      worker.Name,
					JobID:     worker.JobID,
					StartedAt: worker.StartedAt,
				})
			}
			sort.Slice(value.ActiveJobs, func(i, j int) bool { return value.ActiveJobs[i].WorkerID < value.ActiveJobs[j].WorkerID })
			value.ActiveJobCount = len(value.ActiveJobs)
		}
		if a.dependencies.Control != nil {
			probeContext, cancelProbe := a.localProbeContext(ctx)
			controllerStatus, controlErr := a.dependencies.Control.Status(probeContext)
			cancelProbe()
			if controlErr == nil {
				value.ControllerAvailable = true
				value.ActiveJobCount = controllerStatus.ActiveJobCount
				value.Controller = &controllerStatus
			} else {
				controlHealthy = false
			}
		}
		if code := a.writeIndentedJSON(value, "status"); code != ExitOK {
			return code
		}
	} else {
		a.writeHumanStatus(desired, desiredErr, observed, observedErr)
		if a.dependencies.Control != nil {
			probeContext, cancelProbe := a.localProbeContext(ctx)
			_, err := a.dependencies.Control.Status(probeContext)
			cancelProbe()
			if err != nil {
				controlHealthy = false
				writef(a.errOut, "Controller control plane: %v\n", err)
			}
		}
	}
	if !controlHealthy {
		return ExitDegraded
	}
	if observedErr == nil && observed.Phase == model.PhaseDegraded {
		return classifyProblems(observed.Problems)
	}
	return ExitOK
}

type statusDocument struct {
	SchemaVersion       int                  `json:"schemaVersion"`
	ControllerAvailable bool                 `json:"controllerAvailable"`
	ActiveJobCount      int                  `json:"activeJobCount"`
	ActiveJobs          []activeJob          `json:"activeJobs"`
	Controller          *control.Status      `json:"controller,omitempty"`
	Desired             *model.DesiredState  `json:"desired,omitempty"`
	Observed            *model.ObservedState `json:"observed,omitempty"`
}

type activeJob struct {
	WorkerID  string    `json:"workerId"`
	PoolID    string    `json:"poolId"`
	Name      string    `json:"name"`
	JobID     string    `json:"jobId,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

func (a *Application) enable(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host enable", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	wait := flags.Bool("wait", false, "wait until the controller is ready")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	return a.requestMode(ctx, model.ModeEnabled, *wait, false)
}

func (a *Application) disable(ctx context.Context, args []string) int {
	wait, valid := parseWaitDetach("host disable", args, a.errOut)
	if !valid {
		return ExitUsage
	}
	return a.requestMode(ctx, model.ModeDisabled, wait, true)
}

func (a *Application) game(ctx context.Context, args []string) int {
	wait, valid := parseWaitDetach("host game", args, a.errOut)
	if !valid {
		return ExitUsage
	}
	if a.dependencies.Gaming == nil {
		writeln(a.errOut, "gaming host inventory is unavailable")
		return ExitInvalidConfig
	}
	observed, _ := a.dependencies.Store.LoadObserved(ctx)
	inventory := a.dependencies.Gaming.Inventory(ctx)
	a.writeGamingInventory(observed, inventory)
	confirmed, err := a.confirm("Enter gaming mode after active CI work drains? [y/N]: ", "y", "yes")
	if err != nil {
		writef(a.errOut, "read confirmation: %v\n", err)
		return ExitRuntime
	}
	if !confirmed {
		writeln(a.out, "Gaming mode was not requested.")
		return ExitOK
	}
	return a.requestMode(ctx, model.ModeGaming, wait, true)
}

func (a *Application) requestMode(ctx context.Context, mode model.Mode, wait, detachOnCancel bool) int {
	desired, err := a.dependencies.Store.LoadDesired(ctx)
	if errors.Is(err, state.ErrNotFound) {
		desired = model.DesiredState{SchemaVersion: 1}
	} else if err != nil {
		writef(a.errOut, "read desired state: %v\n", err)
		return ExitRuntime
	}
	desired.SchemaVersion = 1
	desired.Mode = mode
	desired.UpdatedAt = a.dependencies.Now().UTC()
	if err := a.dependencies.Store.SaveDesired(ctx, desired); err != nil {
		writef(a.errOut, "write desired state: %v\n", err)
		return ExitRuntime
	}
	writef(a.out, "Requested %s mode.\n", mode)
	if !wait {
		return ExitOK
	}
	return a.waitForMode(ctx, mode, detachOnCancel)
}

func parseWaitDetach(name string, args []string, errOut io.Writer) (bool, bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(errOut)
	wait := flags.Bool("wait", false, "wait for completion")
	detach := flags.Bool("detach", false, "return after recording desired state")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*wait && *detach) {
		if *wait && *detach {
			writeln(errOut, "--wait and --detach are mutually exclusive")
		}
		return false, false
	}
	if !*wait && !*detach {
		return true, true
	}
	return *wait, true
}

func (a *Application) waitForMode(ctx context.Context, mode model.Mode, detachOnCancel bool) int {
	started := a.dependencies.Now()
	warned := false
	for {
		observed, err := a.dependencies.Store.LoadObserved(ctx)
		if err == nil {
			if observed.Phase == model.PhaseDegraded {
				for _, problem := range observed.Problems {
					writef(a.errOut, "%s: %s\n", problem.Code, problem.Message)
				}
				return classifyProblems(observed.Problems)
			}
			if modeReached(mode, observed.Phase) {
				writef(a.out, "Host reached %s phase.\n", observed.Phase)
				return ExitOK
			}
		} else if detachOnCancel && errors.Is(err, context.Canceled) {
			writeln(a.out, "Detached; the controller will continue the requested transition.")
			return ExitOK
		} else if !errors.Is(err, state.ErrNotFound) {
			writef(a.errOut, "read observed state: %v\n", err)
			return ExitRuntime
		}
		if !warned && (mode == model.ModeDisabled || mode == model.ModeGaming) && a.dependencies.Config.Drain.WarningAfter.Duration > 0 && a.dependencies.Now().Sub(started) >= a.dependencies.Config.Drain.WarningAfter.Duration {
			warned = true
			writef(a.errOut, "Drain has exceeded %s; active jobs will continue and will not be terminated automatically.\n", a.dependencies.Config.Drain.WarningAfter.Duration)
		}
		timer := time.NewTimer(a.dependencies.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if detachOnCancel {
				writeln(a.out, "Detached; the controller will continue the requested transition.")
				return ExitOK
			}
			writef(a.errOut, "wait interrupted: %v\n", ctx.Err())
			return ExitRuntime
		case <-timer.C:
		}
	}
}

func modeReached(mode model.Mode, phase model.Phase) bool {
	switch mode {
	case model.ModeEnabled:
		return phase == model.PhaseReady
	case model.ModeDisabled:
		return phase == model.PhaseDisabled
	case model.ModeGaming:
		return phase == model.PhaseGaming
	default:
		return false
	}
}

func classifyProblems(problems []model.Problem) int {
	for _, problem := range problems {
		code := strings.ToLower(problem.Code)
		switch {
		case strings.Contains(code, "credential"), strings.Contains(code, "secret"), strings.Contains(code, "auth"):
			return ExitCredential
		case strings.Contains(code, "timeout"), strings.Contains(code, "timed-out"):
			return ExitOperationTimedOut
		}
	}
	return ExitDegraded
}

func (a *Application) runSecret(ctx context.Context, args []string) int {
	if len(args) == 0 || args[0] != "import" {
		writeln(a.errOut, "usage: ci-runner secret import --file PATH")
		return ExitUsage
	}
	flags := flag.NewFlagSet("secret import", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	file := flags.String("file", "", "RSA GitHub App private-key PEM (removed after a verified protected import)")
	secretID := flags.String("id", "", "configured credential ID (prompted when more than one exists)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(*file) == "" {
		return ExitUsage
	}
	if a.dependencies.Secrets == nil {
		writeln(a.errOut, "secret importer is unavailable")
		return ExitInvalidConfig
	}
	configuredIDs := make(map[string]struct{})
	for _, target := range a.dependencies.Config.GitHub.Targets {
		configuredIDs[target.SecretID] = struct{}{}
	}
	ids := make([]string, 0, len(configuredIDs))
	for id := range configuredIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	selectedID := strings.TrimSpace(*secretID)
	if selectedID == "" && len(ids) == 1 {
		selectedID = ids[0]
	}
	if selectedID == "" && len(ids) > 1 {
		writef(a.out, "Configured credential IDs: %s\nSecret ID: ", strings.Join(ids, ", "))
		selection, readErr := a.readLine()
		if readErr != nil {
			writef(a.errOut, "read secret ID: %v\n", readErr)
			return ExitRuntime
		}
		selectedID = selection
	}
	if _, configured := configuredIDs[selectedID]; !configured || selectedID == "" {
		writef(a.errOut, "secret ID %q is not configured for this host\n", selectedID)
		return ExitInvalidConfig
	}
	destination := filepath.Join(a.dependencies.Config.Paths.Secrets, selectedID+".dpapi")
	result, err := a.dependencies.Secrets.Import(ctx, *file, destination)
	if err != nil {
		writef(a.errOut, "import secret: %v\n", err)
		return ExitCredential
	}
	writef(a.out, "Imported GitHub App key\nPlaintext source PEM removed with identity-bound ordinary filesystem deletion (not media sanitization): %s\nGitHub App fingerprint (Base64 SHA-256): %s\nProtected path: %s\nImported: %s\n", *file, result.Fingerprint, result.Path, result.ImportedAt.Format(time.RFC3339))
	return ExitOK
}

func (a *Application) forceStop(ctx context.Context, args []string) int {
	if len(args) != 0 {
		return ExitUsage
	}
	if a.dependencies.ForceStop == nil {
		writeln(a.errOut, "force-stop runtime is unavailable")
		return ExitInvalidConfig
	}
	if code := a.requestMode(ctx, model.ModeDisabled, false, false); code != ExitOK {
		return code
	}
	desired, err := a.dependencies.Store.LoadDesired(ctx)
	if err != nil {
		writef(a.errOut, "read force-stop desired state: %v\n", err)
		return ExitRuntime
	}
	if code := a.waitForZeroCapacity(ctx, desired.UpdatedAt); code != ExitOK {
		return code
	}
	preview, err := a.dependencies.ForceStop.Preview(ctx)
	if err != nil {
		writef(a.errOut, "inventory force-stop targets: %v\n", err)
		return ExitRuntime
	}
	if len(preview) == 0 {
		writeln(a.out, "No active managed workers require force-stop.")
		return ExitOK
	}
	writeln(a.out, "Force-stop will terminate these managed workers and any jobs they are running:")
	for _, target := range preview {
		writef(a.out, "- %s pool=%s state=%s job=%s\n", target.Name, target.PoolID, target.State, displayValue(target.JobID))
	}
	write(a.out, "Type FORCE STOP to continue: ")
	confirmation, err := a.readLine()
	if err != nil {
		writef(a.errOut, "read confirmation: %v\n", err)
		return ExitRuntime
	}
	if confirmation != "FORCE STOP" {
		writeln(a.out, "Force-stop was not executed; disabled mode remains requested.")
		return ExitOK
	}
	stopped, err := a.dependencies.ForceStop.Execute(ctx, preview)
	if errors.Is(err, controller.ErrForceStopStateChanged) {
		writeln(a.errOut, "Worker state changed after confirmation; nothing was force-stopped. Review the new inventory and try again.")
		return ExitStateChanged
	}
	if err != nil {
		writef(a.errOut, "force-stop workers: %v\n", err)
		return ExitRuntime
	}
	writef(a.out, "Force-stopped %d managed worker(s).\n", len(stopped))
	return ExitOK
}

func (a *Application) waitForZeroCapacity(ctx context.Context, requestedAt time.Time) int {
	for {
		observed, err := a.dependencies.Store.LoadObserved(ctx)
		if err == nil {
			zero := !observed.HeartbeatAt.Before(requestedAt) && (observed.Phase == model.PhaseDraining || observed.Phase == model.PhaseDisabled)
			seen := make(map[string]bool, len(observed.Pools))
			for _, pool := range observed.Pools {
				seen[pool.ID] = true
				if pool.MaxCapacity != 0 {
					zero = false
				}
			}
			for _, target := range a.dependencies.Config.GitHub.Targets {
				if !seen[target.ID] {
					zero = false
				}
			}
			if zero {
				return ExitOK
			}
			if observed.Phase == model.PhaseDegraded {
				return classifyProblems(observed.Problems)
			}
		} else if !errors.Is(err, state.ErrNotFound) {
			writef(a.errOut, "read observed state: %v\n", err)
			return ExitRuntime
		}
		timer := time.NewTimer(a.dependencies.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			writeln(a.errOut, "force-stop canceled while waiting for listeners to advertise zero capacity")
			return ExitRuntime
		case <-timer.C:
		}
	}
}

func (a *Application) confirm(prompt string, accepted ...string) (bool, error) {
	write(a.out, prompt)
	value, err := a.readLine()
	if err != nil {
		return false, err
	}
	for _, candidate := range accepted {
		if strings.EqualFold(value, candidate) {
			return true, nil
		}
	}
	return false, nil
}

func (a *Application) readLine() (string, error) {
	value, err := a.in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && value == "" {
		return "", io.EOF
	}
	return strings.TrimSpace(value), nil
}

func (a *Application) usage() {
	writeln(a.out, `Usage:
  ci-runner host
  ci-runner host status [--json]
  ci-runner host enable [--wait]
  ci-runner host disable [--wait|--detach]
  ci-runner host game [--wait|--detach]
  ci-runner host doctor [--json] [--include-elevated]
  ci-runner host logs [--follow|--job ID]
  ci-runner host force-stop
	ci-runner host controller restart
	ci-runner host controller stop-for-update
  ci-runner secret import --file PATH
  ci-runner [--config PATH] config validate [--json]`)
}

func (a *Application) localProbeContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, a.dependencies.Config.Controller.LocalProbeTimeout.Duration)
}

func displayValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func parseCapacity(value string) (*int, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "reset") {
		return nil, nil
	}
	capacity, err := strconv.Atoi(value)
	if err != nil || capacity < 0 {
		return nil, errors.New("capacity must be a non-negative integer or reset")
	}
	return &capacity, nil
}
