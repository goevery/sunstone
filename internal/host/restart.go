package host

import (
	"context"
	"errors"
	"fmt"
)

// Restart restarts the workload named workloadName on this VM, driven solely
// by deployed state: a missing workload is an error, several containers are
// refused as ambiguous before any mutation, and containers without valid
// deployed metadata are refused safely. A background workload stops
// gracefully if running, starts the same container, and verifies health,
// never pulling images or resolving secrets. A deployed HTTP workload
// creates a stopped replacement cloned from the current container's actual
// inspection, switches traffic to it after readiness and draining, and
// removes the previous container only after the successful switch.
func (h *Host) Restart(ctx context.Context, workloadName string) (Result, error) {
	containers, err := h.runtime.WorkloadContainers(ctx, workloadName)
	if err != nil {
		return Result{}, fmt.Errorf("find workload containers: %w", err)
	}
	switch {
	case len(containers) == 0:
		return Result{}, errors.New("workload is missing")
	case len(containers) > 1:
		return Result{}, fmt.Errorf("workload has %d managed containers; refusing ambiguous state", len(containers))
	}

	current := containers[0]
	if current.Metadata == nil {
		return Result{}, fmt.Errorf("container %s has missing or invalid deployed metadata; refusing restart", current.ID)
	}
	if current.Metadata.Kind == KindBackground {
		return h.restartBackground(ctx, current)
	}

	return h.restartHTTP(ctx, workloadName, current)
}

// restartBackground restarts the existing container in place, preserving its
// image and environment without any pull or secret resolution.
func (h *Host) restartBackground(ctx context.Context, current Container) (Result, error) {
	if current.Running {
		if err := h.runtime.Stop(ctx, current); err != nil {
			return Result{}, fmt.Errorf("stop container: %w", err)
		}
	}

	if err := h.runtime.Start(ctx, current); err != nil {
		return Result{}, fmt.Errorf("start container: %w", err)
	}

	if err := h.runtime.Verify(ctx, current); err != nil {
		// A verification read that loses the transport leaves the final
		// state after the restart unconfirmed; a definitive health failure
		// is reported as-is.
		if transportLost(err) {
			return Result{}, unconfirmed("verify restarted container", fmt.Errorf("verify container: %w", err))
		}

		return Result{}, fmt.Errorf("verify container: %w", err)
	}

	h.report(Progress{Stage: StageRestarted})

	return Result{Container: current.ID, Changed: true}, nil
}

// restartHTTP replaces the deployed container with a snapshot clone and
// switches traffic to it, retaining the previous container until the
// replacement is routed and drained.
func (h *Host) restartHTTP(ctx context.Context, workloadName string, current Container) (Result, error) {
	replacement, err := h.runtime.RestartReplacement(ctx, current)
	if err != nil {
		return Result{}, fmt.Errorf("create replacement: %w", err)
	}

	if err := h.runtime.Start(ctx, replacement); err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "start", err)
	}

	address, err := h.runtime.BackendAddress(ctx, replacement, current.Metadata.HTTPPort)
	if err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "find HTTP backend", err)
	}

	route := Route{Workload: workloadName, Address: address, ContainerID: replacement.ID, StartupProbePath: current.Metadata.StartupProbePath}
	if err := h.runtime.UpdateRoute(ctx, route); err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "update route", err)
	}

	if err := h.runtime.Remove(ctx, current); err != nil {
		return Result{}, fmt.Errorf("remove previous container after draining: %w", err)
	}

	h.report(Progress{Stage: StageRestarted})

	return Result{Container: replacement.ID, Changed: true}, nil
}
