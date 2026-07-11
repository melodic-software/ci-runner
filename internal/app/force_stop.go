package app

import (
	"context"
	"errors"

	"github.com/melodic-software/ci-runner/internal/control"
	"github.com/melodic-software/ci-runner/internal/controller"
)

type controlForceStopClient interface {
	ForceStopPreview(context.Context) ([]control.ForceStopTarget, error)
	ForceStopExecute(context.Context, []control.ForceStopTarget) ([]control.ForceStopTarget, error)
}

type ControlForceStopper struct {
	Client controlForceStopClient
}

func (f ControlForceStopper) Preview(ctx context.Context) ([]controller.ForceStopTarget, error) {
	if f.Client == nil {
		return nil, errors.New("controller force-stop client is unavailable")
	}
	targets, err := f.Client.ForceStopPreview(ctx)
	return fromControlTargets(targets), err
}

func (f ControlForceStopper) Execute(ctx context.Context, expected []controller.ForceStopTarget) ([]controller.ForceStopTarget, error) {
	if f.Client == nil {
		return nil, errors.New("controller force-stop client is unavailable")
	}
	targets, err := f.Client.ForceStopExecute(ctx, toControlTargets(expected))
	var responseErr *control.ResponseError
	if errors.As(err, &responseErr) && responseErr.Code == "force-stop-state-changed" {
		err = controller.ErrForceStopStateChanged
	}
	return fromControlTargets(targets), err
}

func toControlTargets(targets []controller.ForceStopTarget) []control.ForceStopTarget {
	result := make([]control.ForceStopTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, control.ForceStopTarget{
			WorkerID: target.WorkerID, PoolID: target.PoolID, Name: target.Name, State: target.State, JobID: target.JobID,
		})
	}
	return result
}

func fromControlTargets(targets []control.ForceStopTarget) []controller.ForceStopTarget {
	result := make([]controller.ForceStopTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, controller.ForceStopTarget{
			WorkerID: target.WorkerID, PoolID: target.PoolID, Name: target.Name, State: target.State, JobID: target.JobID,
		})
	}
	return result
}
