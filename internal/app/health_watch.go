package app

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/melodic-software/ci-runner/internal/buildinfo"
	"github.com/melodic-software/ci-runner/internal/healthwatch"
	"github.com/melodic-software/ci-runner/internal/host"
)

func (a *Application) healthWatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.healthWatchUsage()
		return ExitUsage
	}
	switch args[0] {
	case "check":
		return a.healthWatchCheck(ctx, args[1:])
	case "install-task":
		return a.healthWatchInstallTask(ctx, args[1:])
	default:
		writef(a.errOut, "unknown health-watch command %q\n", args[0])
		a.healthWatchUsage()
		return ExitUsage
	}
}

func (a *Application) healthWatchCheck(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host health-watch check", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	jsonOutput := flags.Bool("json", false, "write machine-readable health watch result")
	noAlert := flags.Bool("no-alert", false, "evaluate signals without sending an external alert")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}

	if a.dependencies.Jobs == nil {
		writeln(a.errOut, "job index store is required for health watch")
		return ExitRuntime
	}
	checker, err := initHealthWatchChecker(healthwatch.Dependencies{
		Now:       a.dependencies.Now,
		Config:    a.dependencies.Config,
		Store:     a.dependencies.Store,
		Inventory: healthwatch.NewDockerInventory(buildinfo.Version),
		JobsFile:  healthwatch.StoreJobsFile{Source: a.dependencies.Jobs},
		Sidecar:   healthwatch.NewFileSidecar(filepath.Join(a.dependencies.Config.Paths.State, "health-watch.json")),
		Alert:     healthwatch.NewWebhookAlerter(a.dependencies.Config.HealthWatchdog.AlertWebhook),
	})
	if err != nil {
		writef(a.errOut, "initialize health watch checker: %v\n", err)
		return ExitRuntime
	}

	var result healthwatch.Result
	if *noAlert {
		result, err = checker.Check(ctx)
	} else {
		result, err = checker.CheckAndAlert(ctx, true)
	}
	if err != nil {
		writef(a.errOut, "health watch check: %v\n", err)
		return ExitRuntime
	}
	if *jsonOutput {
		if code := a.writeIndentedJSON(result, "health watch result"); code != ExitOK {
			return code
		}
		if result.Healthy {
			return ExitOK
		}
		return ExitDegraded
	}
	if result.Healthy {
		writeln(a.out, "Health watch: healthy")
		return ExitOK
	}
	writeln(a.out, "Health watch: unhealthy")
	for _, finding := range result.Findings {
		writef(a.out, "- %s: %s\n", finding.Code, finding.Message)
	}
	return ExitDegraded
}

func (a *Application) healthWatchInstallTask(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host health-watch install-task", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	configPath := flags.String("config", "", "absolute host configuration path")
	executablePath := flags.String("executable", "", "absolute ci-runner executable path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return ExitUsage
	}
	if *configPath == "" {
		writeln(a.errOut, "host health-watch install-task requires --config")
		return ExitUsage
	}
	if err := host.InstallHealthWatchTask(ctx, host.ScheduledTaskCLI{}, *executablePath, *configPath, a.dependencies.Config.HealthWatchdog.CheckInterval.Duration); err != nil {
		writef(a.errOut, "install health watch scheduled task: %v\n", err)
		return ExitRuntime
	}
	writef(a.out, "Installed canonical scheduled task %q.\n", host.HealthWatchTaskName)
	return ExitOK
}

func (a *Application) healthWatchUsage() {
	writeln(a.out, `Usage:
  ci-runner host health-watch check [--json] [--no-alert]
  ci-runner host health-watch install-task --config PATH [--executable PATH]`)
}
