package main

import (
	"context"
	"os"
	"os/signal"
)

import "github.com/melodic-software/ci-runner/internal/app"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(app.RunMain(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
