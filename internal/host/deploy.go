// Package host operates one workload on one VM, owning the container
// lifecycle, routing switch, and rollback for that VM.
package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goevery/sunstone/internal/workload"
)

// restartPolicy is the Docker restart policy for managed containers.
const restartPolicy = "unless-stopped"

// HostConfig contains the host's operation policies. All fields are
// mandatory; the composition root chooses their values.
type HostConfig struct {
	// CleanupTimeout bounds interruption cleanup that runs after the
	// operation context ends. It must be positive.
	CleanupTimeout time.Duration
}

// Result describes the outcome of one workload operation on one VM.
type Result struct {
	Container string
	Changed   bool
}

// Host deploys a workload definition on the VM reachable through its
// runtime collaborator.
type Host struct {
	runtime        Runtime
	report         Reporter
	cleanupTimeout time.Duration
}

// NewHost constructs a host with all required collaborators. Report receives
// one progress fact per lifecycle step; all collaborators and policies are
// mandatory and rejected when unset.
func NewHost(runtime Runtime, report Reporter, config HostConfig) (*Host, error) {
	if runtime == nil {
		return nil, errors.New("runtime is required")
	}
	if report == nil {
		return nil, errors.New("progress reporter is required")
	}
	if config.CleanupTimeout <= 0 {
		return nil, errors.New("cleanup timeout must be positive")
	}

	return &Host{runtime: runtime, report: report, cleanupTimeout: config.CleanupTimeout}, nil
}

// Deploy applies definition to the VM: it lists the workload's managed
// containers, pulls the image, compares the desired configuration with the
// current container, and replaces or repairs it as needed. Background
// workloads replace stop-then-start; HTTP workloads switch traffic to a
// readiness-gated replacement before draining the previous container.
func (h *Host) Deploy(ctx context.Context, definition workload.Definition) (Result, error) {
	containers, err := h.runtime.WorkloadContainers(ctx, definition.Name)
	if err != nil {
		return Result{}, fmt.Errorf("find workload containers: %w", err)
	}
	if len(containers) > 1 {
		return Result{}, fmt.Errorf("workload %s has %d managed containers; refusing ambiguous state", definition.Name, len(containers))
	}

	// Unknown metadata may hide an active HTTP route. Only an HTTP deploy
	// can safely repair it by explicitly replacing the route; reject a
	// background deployment before any mutation, including image pulls.
	if definition.HTTP == nil && len(containers) == 1 && containers[0].Metadata == nil {
		return Result{}, fmt.Errorf("container %s has missing or invalid deployed metadata; refusing background deployment", containers[0].ID)
	}

	h.report(Progress{Stage: StagePulling, Image: definition.Container.Image})
	image, err := h.runtime.Pull(ctx, definition.Container.Image)
	if err != nil {
		return Result{}, fmt.Errorf("pull image %s: %w", definition.Container.Image, err)
	}

	spec := ContainerSpec{
		Name:              definition.Name,
		Workload:          definition.Name,
		Image:             image,
		Command:           definition.Container.Command,
		Environment:       definition.Container.Environment,
		SecretEnvironment: definition.Container.SecretEnvironment,
		RestartPolicy:     restartPolicy,
		HTTP:              definition.HTTP,
	}

	if definition.HTTP != nil {
		return h.deployHTTP(ctx, definition, spec, containers)
	}

	return h.deployBackground(ctx, spec, containers)
}

func (h *Host) deployBackground(ctx context.Context, spec ContainerSpec, containers []Container) (Result, error) {
	var previous *Container
	var rollbackPrevious *Container
	// The route active before any mutation, captured so a failed
	// HTTP-to-background transition can restore exactly what was there.
	var deployedRoute *Route
	if len(containers) == 1 {
		current := containers[0]
		matches, err := h.runtime.Matches(ctx, current, spec)
		if err != nil {
			return Result{}, fmt.Errorf("compare current container: %w", err)
		}
		if matches && current.Running && current.Healthy {
			h.report(Progress{Stage: StageCurrent})

			return Result{Container: current.ID}, nil
		}

		if matches && !current.Running {
			if err := h.runtime.Start(ctx, current); err != nil {
				return Result{}, fmt.Errorf("start current container: %w", err)
			}
			if err := h.runtime.Verify(ctx, current); err != nil {
				// A verification read that loses the transport after a
				// confirmed start leaves the final state unconfirmed.
				if transportLost(err) {
					return Result{}, unconfirmed("verify started container", fmt.Errorf("verify current container: %w", err))
				}

				return Result{}, fmt.Errorf("verify current container: %w", err)
			}

			return Result{Container: current.ID, Changed: true}, nil
		}

		previous = &current
		if current.Running {
			rollbackPrevious = &current
		}
	}

	container, err := h.runtime.Create(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("create replacement: %w", err)
	}

	// A deployed HTTP container being replaced by a background workload
	// must retire its route before it stops serving. The active route is
	// captured for rollback exactly as observed — an absent route stays
	// absent and nothing is invented — and a delete failure leaves the
	// previous container untouched either way. An unclassifiable previous
	// container never reaches this transition: Deploy refuses it before any
	// mutation, so its metadata is valid.
	if previous != nil && previous.Metadata.Kind == KindHTTP {
		route, found, err := h.runtime.Route(ctx, spec.Workload)
		if err != nil {
			return Result{}, h.abandonReplacement(ctx, container, fmt.Errorf("get deployed route: %w", err))
		}
		if found {
			deployedRoute = &route
			if err := h.runtime.DeleteRoute(ctx, spec.Workload, false); err != nil {
				if EffectsUnconfirmed(err) {
					return Result{}, fmt.Errorf("delete deployed route: %w", err)
				}

				return Result{}, h.abandonReplacement(ctx, container, fmt.Errorf("delete deployed route: %w", err))
			}
		}
	}

	if rollbackPrevious != nil {
		if err := h.runtime.Stop(ctx, *rollbackPrevious); err != nil {
			// A stop whose answer was lost may have been applied: removing
			// the replacement could leave nothing running, so the
			// speculative cleanup is skipped and the final state stays
			// unconfirmed.
			if EffectsUnconfirmed(err) {
				return Result{}, fmt.Errorf("stop current container: %w", err)
			}

			// A definitively failed stop leaves the current container
			// running; the stopped replacement is removed as cleanup and,
			// in a transition, the retired route is restored to the still
			// serving previous container. A cleanup failure is never
			// discarded, and an uncertain one stops all further remote work.
			cleanupCtx, cancel := h.cleanupContext(ctx)
			removeErr := h.runtime.Remove(cleanupCtx, container)
			if EffectsUnconfirmed(removeErr) || transportLost(removeErr) {
				cancel()

				return Result{}, unconfirmed("remove failed replacement", errors.Join(fmt.Errorf("stop current container: %w", err), removeErr))
			}
			restoreErr := h.restoreRoute(cleanupCtx, deployedRoute)
			cancel()
			if EffectsUnconfirmed(restoreErr) || transportLost(restoreErr) {
				return Result{}, unconfirmed("restore deployed route", errors.Join(fmt.Errorf("stop current container: %w", err), restoreErr))
			}
			joined := fmt.Errorf("stop current container: %w", err)
			if removeErr != nil {
				joined = fmt.Errorf("%w; remove failed replacement: %w", joined, removeErr)
			}
			if restoreErr != nil {
				joined = fmt.Errorf("%w; restore deployed route: %w", joined, restoreErr)
			}

			return Result{}, joined
		}
	}

	if err := h.runtime.Start(ctx, container); err != nil {
		return Result{}, h.replacementFailure(ctx, container, rollbackPrevious, deployedRoute, "start", err)
	}
	if err := h.runtime.Verify(ctx, container); err != nil {
		return Result{}, h.replacementFailure(ctx, container, rollbackPrevious, deployedRoute, "verify", err)
	}
	if previous != nil {
		if err := h.runtime.Remove(ctx, *previous); err != nil {
			return Result{}, fmt.Errorf("remove previous container after successful replacement: %w", err)
		}
	}

	h.report(Progress{Stage: StageHealthy})

	return Result{Container: container.ID, Changed: true}, nil
}

func (h *Host) deployHTTP(ctx context.Context, definition workload.Definition, spec ContainerSpec, containers []Container) (Result, error) {
	var previous *Container
	if len(containers) == 1 {
		current := containers[0]
		matches, err := h.runtime.Matches(ctx, current, spec)
		if err != nil {
			return Result{}, fmt.Errorf("compare current container: %w", err)
		}
		// Only a healthy current container counts as current: an unhealthy
		// match takes the normal readiness-gated replacement instead of a
		// mere route repair.
		if matches && current.Running && current.Healthy {
			address, err := h.runtime.BackendAddress(ctx, current, definition.HTTP.ContainerPort)
			if err != nil {
				return Result{}, fmt.Errorf("find current HTTP backend: %w", err)
			}
			desiredRoute := Route{Workload: definition.Name, Address: address, ContainerID: current.ID, StartupProbePath: definition.HTTP.StartupProbePath}
			activeRoute, found, err := h.runtime.Route(ctx, definition.Name)
			if err != nil {
				return Result{}, fmt.Errorf("get current route: %w", err)
			}
			if found && activeRoute == desiredRoute {
				h.report(Progress{Stage: StageCurrent})

				return Result{Container: current.ID}, nil
			}

			h.report(Progress{Stage: StageRouting})
			if err := h.runtime.UpdateRoute(ctx, desiredRoute); err != nil {
				return Result{}, fmt.Errorf("update route: %w", err)
			}
			h.report(Progress{Stage: StageDrained})
			h.report(Progress{Stage: StageHealthy})

			return Result{Container: current.ID, Changed: true}, nil
		}

		previous = &current
	}

	replacement, err := h.runtime.Create(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("create replacement: %w", err)
	}
	h.report(Progress{Stage: StageStarting})
	if err := h.runtime.Start(ctx, replacement); err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "start", err)
	}
	address, err := h.runtime.BackendAddress(ctx, replacement, definition.HTTP.ContainerPort)
	if err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "find HTTP backend", err)
	}
	route := Route{Workload: definition.Name, Address: address, ContainerID: replacement.ID, StartupProbePath: definition.HTTP.StartupProbePath}
	h.report(Progress{Stage: StageRouting})
	if err := h.runtime.UpdateRoute(ctx, route); err != nil {
		return Result{}, h.replacementFailure(ctx, replacement, nil, nil, "update route", err)
	}
	h.report(Progress{Stage: StageDrained})
	if previous != nil {
		if err := h.runtime.Remove(ctx, *previous); err != nil {
			return Result{}, fmt.Errorf("remove previous container after draining: %w", err)
		}
	}

	h.report(Progress{Stage: StageHealthy})

	return Result{Container: replacement.ID, Changed: true}, nil
}

// replacementFailure cleans up a failed replacement container and rolls
// the previous container back, reporting every failure it encounters. A
// cause whose remote effects are unconfirmed — or whose transport was lost
// — disables all remote cleanup: the replacement may already exist or even
// serve traffic, so nothing may be removed or restarted on its account.
// Cleanup that itself loses the transport stops the remaining remote
// operations and reports the final state as unconfirmed. When a transition
// retired a previously active route, the rollback restores it only after
// the previous container starts successfully; a nil route restores nothing.
func (h *Host) replacementFailure(ctx context.Context, replacement Container, previous *Container, deployedRoute *Route, phase string, cause error) error {
	if EffectsUnconfirmed(cause) || transportLost(cause) {
		return unconfirmed(fmt.Sprintf("%s replacement", phase), cause)
	}

	cleanupCtx, cancel := h.cleanupContext(ctx)
	defer cancel()
	err := fmt.Errorf("%s replacement: %w", phase, cause)

	logs, logsErr := h.runtime.Logs(cleanupCtx, replacement)
	if EffectsUnconfirmed(logsErr) || transportLost(logsErr) {
		return unconfirmed("capture replacement logs", errors.Join(err, logsErr))
	}
	if logs != "" {
		err = fmt.Errorf("%w\nreplacement logs:\n%s", err, logs)
	}
	if logsErr != nil {
		err = fmt.Errorf("%w; capture replacement logs: %w", err, logsErr)
	}

	removeErr := h.runtime.Remove(cleanupCtx, replacement)
	if EffectsUnconfirmed(removeErr) || transportLost(removeErr) {
		return unconfirmed("remove failed replacement", errors.Join(err, removeErr))
	}
	if removeErr != nil {
		err = fmt.Errorf("%w; remove failed replacement: %w", err, removeErr)
	}

	if previous != nil && removeErr == nil {
		if rollbackErr := h.runtime.Start(cleanupCtx, *previous); rollbackErr != nil {
			if EffectsUnconfirmed(rollbackErr) || transportLost(rollbackErr) {
				return unconfirmed("rollback previous container", errors.Join(err, rollbackErr))
			}
			err = fmt.Errorf("%w; rollback previous container: %w", err, rollbackErr)
		} else {
			// The previous container is serving again, so reconnecting its
			// retired route restores traffic; Sunbeam owns the readiness
			// gate and draining.
			if restoreErr := h.restoreRoute(cleanupCtx, deployedRoute); restoreErr != nil {
				if EffectsUnconfirmed(restoreErr) || transportLost(restoreErr) {
					return unconfirmed("restore deployed route", errors.Join(err, restoreErr))
				}
				err = fmt.Errorf("%w; restore deployed route: %w", err, restoreErr)
			}
		}
	}

	return err
}

// restoreRoute switches Sunbeam back to the route captured before a
// transition's mutations; a nil route means none was active, and none is
// invented.
func (h *Host) restoreRoute(ctx context.Context, deployedRoute *Route) error {
	if deployedRoute == nil {
		return nil
	}

	return h.runtime.UpdateRoute(ctx, *deployedRoute)
}

// abandonReplacement disposes a stopped replacement that never entered
// service after a definitive failure, preserving uncertainty if the
// disposal itself cannot establish its own effects. A cause whose remote
// effects are unconfirmed — or whose transport was lost — disables all
// cleanup, matching replacementFailure's no-recovery policy: the candidate
// stays as it is and the final state is reported unconfirmed.
func (h *Host) abandonReplacement(ctx context.Context, candidate Container, cause error) error {
	if EffectsUnconfirmed(cause) || transportLost(cause) {
		return unconfirmed("abandon unused replacement", cause)
	}

	cleanupCtx, cancel := h.cleanupContext(ctx)
	defer cancel()
	removeErr := h.runtime.Remove(cleanupCtx, candidate)
	if EffectsUnconfirmed(removeErr) || transportLost(removeErr) {
		return unconfirmed("remove unused replacement", errors.Join(cause, removeErr))
	}
	if removeErr != nil {
		return fmt.Errorf("%w; remove unused replacement: %w", cause, removeErr)
	}

	return cause
}

// cleanupContext bounds cleanup work that must not inherit the operation's
// cancellation but may never run unbounded.
func (h *Host) cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), h.cleanupTimeout)
}
