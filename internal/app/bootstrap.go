package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/config"
	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/host"
	"github.com/melodic-software/ci-runner/internal/jobindex"
	dockerruntime "github.com/melodic-software/ci-runner/internal/runtime/docker"
	"github.com/melodic-software/ci-runner/internal/secret"
	statefs "github.com/melodic-software/ci-runner/internal/state/fs"
)

// RunMain loads the one canonical host configuration and builds the concrete
// local adapters used by the interactive and automation command surfaces.
func RunMain(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) > 0 && args[0] == "release" {
		return runReleaseCommand(args[1:], out, errOut)
	}
	configPath, commandArgs, err := resolveConfigArgument(args)
	if err != nil {
		writeln(errOut, err)
		return ExitUsage
	}
	file, err := os.Open(configPath)
	if err != nil {
		return reportConfigLoadFailure(out, errOut, requestsJSONConfigValidation(commandArgs), "open", configPath, err)
	}
	cfg, loadErr := config.Load(file)
	closeErr := file.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		return reportConfigLoadFailure(out, errOut, requestsJSONConfigValidation(commandArgs), "load", configPath, err)
	}
	if len(commandArgs) > 0 && commandArgs[0] == "config" {
		return runConfigCommand(cfg, commandArgs[1:], out, errOut)
	}
	acl := secret.NewAccessController()
	for name, path := range map[string]string{
		"secrets": cfg.Paths.Secrets, "state": cfg.Paths.State,
		"logs": cfg.Paths.Logs, "diagnostics": cfg.Paths.Diagnostics,
	} {
		if err := ensureNoReparsePoints(path); err != nil {
			writef(errOut, "unsafe %s runtime path: %v\n", name, err)
			return ExitInvalidConfig
		}
	}
	locker, err := statefs.NewPlatformLocker(cfg.Paths.State)
	if err != nil {
		writef(errOut, "create state mutex: %v\n", err)
		return ExitInvalidConfig
	}
	store, err := statefs.New(cfg.Paths.State, locker, acl)
	if err != nil {
		writef(errOut, "create state store: %v\n", err)
		return ExitInvalidConfig
	}
	jobs, err := jobindex.NewFileStore(cfg.Paths.State, locker, acl)
	if err != nil {
		writef(errOut, "create job index: %v\n", err)
		return ExitInvalidConfig
	}
	protector := secret.NewDPAPIProtector()
	bitLocker := secret.NewBitLockerVerifier()
	secretImporter := secret.Importer{Protector: protector, BitLocker: bitLocker, ACL: acl}
	secretStore := secret.Store{Protector: protector, Directory: cfg.Paths.Secrets}
	controlClient, err := control.NewCurrentUserClient()
	if err != nil {
		writef(errOut, "create current-user controller client: %v\n", err)
		return ExitInvalidConfig
	}
	application, err := New(Dependencies{
		Config:    cfg,
		Store:     store,
		Gaming:    host.NewPlatformGamingHost(),
		Secrets:   secretImporter,
		ForceStop: ControlForceStopper{Client: controlClient},
		Logs: FileLogs{
			ControllerDirectory: filepath.Join(cfg.Paths.Logs, "controller"),
			WorkerLogDirectory:  filepath.Join(cfg.Paths.Logs, "workers"),
			DiagnosticDirectory: cfg.Paths.Diagnostics,
			Jobs:                jobs,
			Cleaner: LogCleanupFunc(func(ctx context.Context) error {
				artifacts, err := newWorkerArtifactSink(cfg, acl, jobs)
				if err != nil {
					return err
				}
				adopted, err := dockerruntime.InventoryLocalArtifacts(ctx, buildinfo.Version, cfg.Host.ID)
				if err != nil {
					return fmt.Errorf("inventory local workers before cleanup: %w", err)
				}
				return artifacts.CleanupNow(ctx, adopted)
			}),
		},
		Doctor: NewLocalDoctorInspector(cfg, acl, bitLocker, secretStore, func(ctx context.Context) (string, string, error) {
			return dockerruntime.ProbeLocal(ctx, buildinfo.Version)
		}),
		Control:         controlClient,
		Processes:       host.WindowsProcessObserver{},
		Tasks:           host.ScheduledTaskCLI{},
		RestartReceipts: store,
	}, in, out, errOut)
	if err != nil {
		writef(errOut, "initialize application: %v\n", err)
		return ExitInvalidConfig
	}
	return application.Run(ctx, commandArgs)
}

func runConfigCommand(cfg config.Config, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		writeln(errOut, "usage: ci-runner [--config PATH] config validate [--json]")
		return ExitUsage
	}
	flags := flag.NewFlagSet("config validate", flag.ContinueOnError)
	flags.SetOutput(errOut)
	jsonOutput := flags.Bool("json", false, "write machine-readable validation result")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	if *jsonOutput {
		result, err := newConfigValidationResult(cfg)
		if err != nil {
			writef(errOut, "normalize validated configuration: %v\n", err)
			return ExitInvalidConfig
		}
		if err := json.NewEncoder(out).Encode(result); err != nil {
			writef(errOut, "write validation result: %v\n", err)
			return ExitRuntime
		}
	} else {
		writef(out, "Configuration is valid (schema %d, host %s, %d target(s)).\n", cfg.SchemaVersion, cfg.Host.ID, len(cfg.GitHub.Targets))
	}
	return ExitOK
}

type configValidationResult struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Valid         bool                    `json:"valid"`
	HostID        string                  `json:"hostId"`
	TargetCount   int                     `json:"targetCount"`
	Release       configValidationRelease `json:"release"`
	Paths         configValidationPaths   `json:"paths"`
}

type configValidationRelease struct {
	CompatibilityManifest string `json:"compatibilityManifest"`
}

type configValidationPaths struct {
	Secrets     string `json:"secrets"`
	State       string `json:"state"`
	Logs        string `json:"logs"`
	Diagnostics string `json:"diagnostics"`
}

func newConfigValidationResult(cfg config.Config) (configValidationResult, error) {
	canonical := func(name, value string) (string, error) {
		normalized, err := config.CanonicalWindowsLocalPath(value)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return normalized, nil
	}
	manifest, err := canonical("release.compatibilityManifest", cfg.Release.CompatibilityManifest)
	if err != nil {
		return configValidationResult{}, err
	}
	secrets, err := canonical("paths.secrets", cfg.Paths.Secrets)
	if err != nil {
		return configValidationResult{}, err
	}
	statePath, err := canonical("paths.state", cfg.Paths.State)
	if err != nil {
		return configValidationResult{}, err
	}
	logs, err := canonical("paths.logs", cfg.Paths.Logs)
	if err != nil {
		return configValidationResult{}, err
	}
	diagnostics, err := canonical("paths.diagnostics", cfg.Paths.Diagnostics)
	if err != nil {
		return configValidationResult{}, err
	}
	return configValidationResult{
		SchemaVersion: 1,
		Valid:         true,
		HostID:        cfg.Host.ID,
		TargetCount:   len(cfg.GitHub.Targets),
		Release:       configValidationRelease{CompatibilityManifest: manifest},
		Paths: configValidationPaths{
			Secrets: secrets, State: statePath, Logs: logs, Diagnostics: diagnostics,
		},
	}, nil
}

// reportConfigLoadFailure reports a configuration open/load failure in
// whichever form the caller requested: the same minimal JSON validation
// envelope used by both failure points when JSON was requested, or a plain
// "<action> configuration %q: %v" line on stderr otherwise.
func reportConfigLoadFailure(out, errOut io.Writer, jsonRequested bool, action, configPath string, err error) int {
	if jsonRequested {
		_ = json.NewEncoder(out).Encode(struct {
			SchemaVersion int    `json:"schemaVersion"`
			Valid         bool   `json:"valid"`
			Error         string `json:"error"`
		}{SchemaVersion: 1, Valid: false, Error: err.Error()})
	} else {
		writef(errOut, "%s configuration %q: %v\n", action, configPath, err)
	}
	return ExitInvalidConfig
}

func requestsJSONConfigValidation(args []string) bool {
	return len(args) == 3 && args[0] == "config" && args[1] == "validate" && args[2] == "--json"
}

func resolveConfigArgument(args []string) (string, []string, error) {
	path := os.Getenv("CI_RUNNER_CONFIG")
	if path == "" {
		var err error
		path, err = defaultConfigPath()
		if err != nil {
			return "", nil, err
		}
	}
	if len(args) == 0 {
		return path, args, nil
	}
	switch {
	case args[0] == "--config":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return "", nil, errors.New("--config requires an absolute path")
		}
		path = args[1]
		args = args[2:]
	case strings.HasPrefix(args[0], "--config="):
		path = strings.TrimPrefix(args[0], "--config=")
		if strings.TrimSpace(path) == "" {
			return "", nil, errors.New("--config requires an absolute path")
		}
		args = args[1:]
	}
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("configuration path must be absolute")
	}
	return filepath.Clean(path), args, nil
}

func defaultConfigPath() (string, error) {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "ci-runner", "config.yaml"), nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve local configuration directory: %w", err)
	}
	return filepath.Join(base, "ci-runner", "config.yaml"), nil
}
