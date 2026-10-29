package host

import (
	"context"
	"fmt"
)

// Check verifies this VM can serve a mutation of workloadName without
// changing anything. Listing the workload's containers proves Docker API
// access. Sunbeam control access is verified only when routing is needed on
// this target: when the desired configuration is HTTP, or when any deployed
// container records HTTP metadata, which covers HTTP-to-background and
// background-to-HTTP transitions and lets name-only operations follow
// deployed state instead of configuration. Reading the workload's route —
// found or missing — proves reachability; a missing route is not a failure.
// Check never pulls images, fetches secrets, probes startup, or repairs
// metadata: a managed container without valid metadata is unclassifiable, so
// every operation that cannot repair it — a background deployment or a
// name-only restart or removal — is refused here before any target is
// mutated. Only a desired HTTP deployment is permitted past missing
// metadata, because it repairs the route explicitly.
func (h *Host) Check(ctx context.Context, workloadName string, desiredHTTP bool) error {
	containers, err := h.runtime.WorkloadContainers(ctx, workloadName)
	if err != nil {
		return fmt.Errorf("check Docker access: %w", err)
	}

	needsRouting := desiredHTTP
	for _, container := range containers {
		if container.Metadata == nil {
			if !desiredHTTP {
				return fmt.Errorf("container %s has missing or invalid deployed metadata; refusing operations that cannot repair it", container.ID)
			}

			continue
		}
		if container.Metadata.Kind == KindHTTP {
			needsRouting = true
		}
	}
	if !needsRouting {
		return nil
	}

	if _, _, err := h.runtime.Route(ctx, workloadName); err != nil {
		return fmt.Errorf("check Sunbeam access: %w", err)
	}

	return nil
}
