package app

import (
	"context"
	"flag"
)

func (a *Application) reboot(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("host reboot", flag.ContinueOnError)
	flags.SetOutput(a.errOut)
	timeout := flags.Duration("timeout", 0, "bound the drain wait")
	force := flags.Bool("force", false, "reboot after an unclean drain")
	dryRun := flags.Bool("dry-run", false, "drain and report without restarting")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		writeln(a.errOut, "usage: ci-runner host reboot [--timeout DURATION] [--force] [--dry-run]")
		return ExitUsage
	}
	if *timeout < 0 {
		writeln(a.errOut, "--timeout must be non-negative")
		return ExitUsage
	}
	if !*dryRun && a.dependencies.Reboot == nil {
		writeln(a.errOut, "host reboot adapter is unavailable")
		return ExitInvalidConfig
	}

	drainCtx := ctx
	if *timeout > 0 {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	drainCode := a.stopController(drainCtx, false)
	if drainCode != ExitOK && !*force {
		writeln(a.errOut, "drain did not complete cleanly; pass --force to reboot anyway")
		return drainCode
	}
	if drainCode != ExitOK {
		writeln(a.errOut, "drain did not complete cleanly; --force continues to reboot")
	}
	if *dryRun {
		writeln(a.out, "Dry run: drain finished; machine restart was not requested.")
		return ExitOK
	}

	if err := a.dependencies.Reboot.Reboot(ctx, 0, "ci-runner host reboot"); err != nil {
		writef(a.errOut, "request machine restart: %v\n", err)
		return ExitRuntime
	}
	writeln(a.out, "Machine restart requested after the capacity-zero drain.")
	return ExitOK
}
