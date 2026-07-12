package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/melodic-software/ci-runner/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := app.RunControllerMain(ctx, os.Args[1:], os.Stderr)
	if err == nil {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(controllerExitCode(err))
}

func controllerExitCode(err error) int {
	if errors.Is(err, app.ErrControllerRestartRequested) {
		return int(app.ControllerRestartExitCode)
	}
	return 1
}
