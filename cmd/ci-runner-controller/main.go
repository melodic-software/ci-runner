package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/melodic-software/ci-runner/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := app.RunControllerMain(ctx, os.Args[1:], os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
