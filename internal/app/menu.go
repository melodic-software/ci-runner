package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/melodic-software/ci-runner/internal/model"
	"github.com/melodic-software/ci-runner/internal/state"
)

func (a *Application) menu(ctx context.Context) int {
	for {
		fmt.Fprintln(a.out, `
ci-runner host
  1. Status and health
  2. Enable/resume CI
  3. Drain and disable CI
  4. Gaming mode
  5. Doctor
  6. Logs
  7. Temporary capacity
  8. Force stop
  9. Exit`)
		fmt.Fprint(a.out, "Select an option: ")
		choice, err := a.readLine()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return ExitOK
			}
			fmt.Fprintf(a.errOut, "read menu selection: %v\n", err)
			return ExitRuntime
		}
		switch choice {
		case "1":
			a.status(ctx, nil)
		case "2":
			a.enable(ctx, []string{"--wait"})
		case "3":
			a.disable(ctx, []string{"--wait"})
		case "4":
			a.game(ctx, []string{"--wait"})
		case "5":
			a.doctor(ctx, nil)
		case "6":
			a.logs(ctx, nil)
		case "7":
			a.temporaryCapacity(ctx)
		case "8":
			a.forceStop(ctx, nil)
		case "9", "q", "quit", "exit":
			return ExitOK
		default:
			fmt.Fprintln(a.errOut, "Choose a number from 1 through 9.")
		}
	}
}

func (a *Application) temporaryCapacity(ctx context.Context) int {
	desired, err := a.dependencies.Store.LoadDesired(ctx)
	if errors.Is(err, state.ErrNotFound) {
		desired = model.DesiredState{SchemaVersion: 1, Mode: model.ModeDisabled}
	} else if err != nil {
		fmt.Fprintf(a.errOut, "read desired state: %v\n", err)
		return ExitRuntime
	}
	if desired.TemporaryCapacityOverride == nil {
		fmt.Fprintln(a.out, "Temporary capacity currently uses the checked-in configuration.")
	} else {
		fmt.Fprintf(a.out, "Temporary capacity is %d.\n", *desired.TemporaryCapacityOverride)
	}
	fmt.Fprint(a.out, "Enter a non-negative capacity, or reset: ")
	value, err := a.readLine()
	if err != nil {
		fmt.Fprintf(a.errOut, "read capacity: %v\n", err)
		return ExitRuntime
	}
	capacity, err := parseCapacity(value)
	if err != nil {
		fmt.Fprintln(a.errOut, err)
		return ExitUsage
	}
	desired.TemporaryCapacityOverride = capacity
	desired.UpdatedAt = a.dependencies.Now().UTC()
	if err := a.dependencies.Store.SaveDesired(ctx, desired); err != nil {
		fmt.Fprintf(a.errOut, "write desired state: %v\n", err)
		return ExitRuntime
	}
	if capacity == nil {
		fmt.Fprintln(a.out, "Temporary capacity reset to the checked-in value.")
	} else {
		fmt.Fprintf(a.out, "Temporary capacity set to %d; admission and pressure gates still apply.\n", *capacity)
	}
	return ExitOK
}
