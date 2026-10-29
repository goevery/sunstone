package host

import (
	"context"
	"fmt"
)

// Remove deletes the workload named workloadName from this VM, driven solely
// by deployed metadata: all managed candidates are validated before any
// mutation, the workload-keyed Sunbeam route is drained first when any
// candidate is deployed HTTP (background-only removal never contacts
// Sunbeam), and every candidate is removed without choosing a winner. An
// empty candidate set is a successful no-op. The first failure halts with
// context and leaves later candidates untouched.
func (h *Host) Remove(ctx context.Context, workloadName string) (Result, error) {
	containers, err := h.runtime.WorkloadContainers(ctx, workloadName)
	if err != nil {
		return Result{}, fmt.Errorf("find workload containers: %w", err)
	}

	// Validate every candidate before touching a route or container.
	for _, container := range containers {
		if container.Metadata == nil {
			return Result{}, fmt.Errorf("container %s has missing or invalid deployed metadata; refusing removal", container.ID)
		}
	}

	if anyDeployedHTTP(containers) {
		if err := h.runtime.DeleteRoute(ctx, workloadName, true); err != nil {
			return Result{}, fmt.Errorf("delete route: %w", err)
		}
	}

	for _, container := range containers {
		// Drained HTTP candidates are removed without stopping; running
		// background candidates stop gracefully first.
		if container.Metadata.Kind == KindBackground && container.Running {
			if err := h.runtime.Stop(ctx, container); err != nil {
				return Result{}, fmt.Errorf("stop container %s: %w", container.ID, err)
			}
		}

		if err := h.runtime.Remove(ctx, container); err != nil {
			return Result{}, fmt.Errorf("remove container %s: %w", container.ID, err)
		}
	}

	h.report(Progress{Stage: StageRemoved})

	return Result{Changed: len(containers) != 0}, nil
}
