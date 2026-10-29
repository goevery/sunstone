package host

import "context"

// Observed workload states for one VM.
const (
	StateMissing         = "missing"
	StateAmbiguous       = "ambiguous"
	StateInvalidMetadata = "invalid-metadata"
	StateStopped         = "stopped"
	StateUnhealthy       = "unhealthy"
	StateRouteMissing    = "route-missing"
	StateRouteMismatch   = "route-mismatch"
	StateCurrent         = "current"
)

// Observation is the observed state of one workload on one VM.
type Observation struct {
	State      string
	Containers []Container
	Route      *Route
}

// Status observes the deployed state of the workload named workloadName on
// this VM. It reads only deployed container metadata and, for deployed HTTP
// workloads, the active Sunbeam route; it never pulls images, resolves
// secrets, or mutates anything. Classification comes from deployed metadata,
// never from desired configuration: zero containers are missing, several
// containers are ambiguous without choosing one, and a container with
// missing or malformed metadata is reported as invalid rather than guessed.
// An ambiguous set still reports the workload's active route when any
// container shows valid deployed HTTP metadata, because the route is keyed
// by workload and observing it requires no choice among containers.
func (h *Host) Status(ctx context.Context, workloadName string) (Observation, error) {
	containers, err := h.runtime.WorkloadContainers(ctx, workloadName)
	if err != nil {
		return Observation{}, err
	}
	observation := Observation{Containers: containers}

	if len(containers) == 0 {
		observation.State = StateMissing

		return observation, nil
	}

	if len(containers) > 1 {
		observation.State = StateAmbiguous
		if anyDeployedHTTP(containers) {
			route, found, routeErr := h.runtime.Route(ctx, workloadName)
			if routeErr != nil {
				return observation, routeErr
			}
			if found {
				observation.Route = &route
			}
		}

		return observation, nil
	}

	container := containers[0]
	if container.Metadata == nil {
		observation.State = StateInvalidMetadata

		return observation, nil
	}

	// Route observation requires a confidently classified HTTP workload;
	// background observations never touch Sunbeam.
	if container.Metadata.Kind == KindHTTP {
		route, found, routeErr := h.runtime.Route(ctx, workloadName)
		if routeErr != nil {
			return observation, routeErr
		}
		if found {
			observation.Route = &route
		}
	}

	switch {
	case !container.Running:
		observation.State = StateStopped
	case !container.Healthy:
		observation.State = StateUnhealthy
	case container.Metadata.Kind != KindHTTP:
		observation.State = StateCurrent
	case observation.Route == nil:
		observation.State = StateRouteMissing
	case observation.Route.ContainerID != container.ID:
		observation.State = StateRouteMismatch
	default:
		observation.State = StateCurrent
	}

	return observation, nil
}

// anyDeployedHTTP reports whether any observed container carries valid
// deployed HTTP metadata, meaning the workload has a Sunbeam route to read.
func anyDeployedHTTP(containers []Container) bool {
	for _, container := range containers {
		if container.Metadata != nil && container.Metadata.Kind == KindHTTP {
			return true
		}
	}

	return false
}
