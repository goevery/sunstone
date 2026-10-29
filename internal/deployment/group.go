package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/workload"
)

// Config contains the group's operation policies. All fields are mandatory;
// the composition root chooses their values.
type Config struct {
	// PreflightTimeout bounds each target's preflight check. It must be
	// positive.
	PreflightTimeout time.Duration
}

// Group coordinates one operation across the selected target VMs of one or
// more workloads. Mutations open and check every selected target before
// changing anything, then apply sequentially and stop at the first failure,
// reporting every selected target; status is best-effort across all
// targets. The group never opens connections itself: the provider owns
// target access and the cleanup of what it opens.
type Group struct {
	provider         Provider
	report           host.Reporter
	preflightTimeout time.Duration
}

// NewGroup constructs the host group with all required collaborators.
func NewGroup(provider Provider, report host.Reporter, config Config) (*Group, error) {
	if provider == nil {
		return nil, errors.New("host provider is required")
	}
	if report == nil {
		return nil, errors.New("progress reporter is required")
	}
	if config.PreflightTimeout <= 0 {
		return nil, errors.New("preflight timeout must be positive")
	}

	return &Group{provider: provider, report: report, preflightTimeout: config.PreflightTimeout}, nil
}

// Deploy deploys definition to each of its configured targets. Every target
// is opened and checked before any mutation: a failing preflight fails the
// failing targets and mutates nothing anywhere. A successful preflight's
// prepared handles are then applied sequentially, stopping at the first
// failure; every selected target is reported.
func (g *Group) Deploy(ctx context.Context, definition workload.Definition) (WorkloadResult, error) {
	results, err := g.operate(ctx, []Selection{SelectionOf(definition)}, "deploy", definition.HTTP != nil, func(ctx context.Context, targetHost Host, _ Selection) (host.Result, error) {
		return targetHost.Deploy(ctx, definition)
	})

	return results[0], err
}

// Restart restarts each selected workload on its targets. All selections are
// opened and checked before any restart; apply then runs sequentially and
// stops at the first failure.
func (g *Group) Restart(ctx context.Context, selections []Selection) ([]WorkloadResult, error) {
	results, err := g.operate(ctx, selections, "restart", false, func(ctx context.Context, targetHost Host, selection Selection) (host.Result, error) {
		return targetHost.Restart(ctx, selection.Workload)
	})

	return results, err
}

// Remove removes each selected workload from its targets. All selections are
// opened and checked before any removal; apply then runs sequentially and
// stops at the first failure.
func (g *Group) Remove(ctx context.Context, selections []Selection) ([]WorkloadResult, error) {
	results, err := g.operate(ctx, selections, "remove", false, func(ctx context.Context, targetHost Host, selection Selection) (host.Result, error) {
		return targetHost.Remove(ctx, selection.Workload)
	})

	return results, err
}

// Status observes every selected workload on all its targets, best-effort:
// each target keeps its observation even when opening or observing it
// fails, and the returned error joins every target failure. Status emits no
// progress, never stops early, and does not preflight.
func (g *Group) Status(ctx context.Context, selections []Selection) ([]WorkloadStatus, error) {
	results := make([]WorkloadStatus, len(selections))
	var statusErr error
	for selectionIndex, selection := range selections {
		result := WorkloadStatus{Workload: selection.Workload, Targets: make([]TargetStatus, len(selection.Targets))}
		for targetIndex, target := range selection.Targets {
			observed := TargetStatus{Target: target}
			if err := ctx.Err(); err != nil {
				observed.Observation = host.Observation{State: StateUnreachable}
				observed.Error = err
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: %w", selection.Workload, target.Instance, err))
				result.Targets[targetIndex] = observed

				continue
			}

			handle, err := g.provider.Open(ctx, target, g.report)
			if err != nil {
				observed.Observation = host.Observation{State: StateUnreachable}
				observed.Error = fmt.Errorf("connect: %w", err)
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: connect: %w", selection.Workload, target.Instance, err))
				result.Targets[targetIndex] = observed

				continue
			}

			observation, observeErr := handle.Host.Status(ctx, selection.Workload)
			closeErr := handle.Close()
			observed.Observation = observation

			if observeErr == nil {
				// A successful observation stands even when only closing the
				// target failed locally; the cleanup error is still reported.
				observed.Error = closeErr
				if closeErr != nil {
					statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: close target: %w", selection.Workload, target.Instance, closeErr))
				}
			} else {
				// A host failure keeps whatever the host could still classify;
				// unreachable is claimed only when the observation provides no
				// meaningful state.
				observed.Error = errors.Join(observeErr, closeErr)
				if observation.State == "" {
					observed.Observation.State = StateUnreachable
				}
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: %w", selection.Workload, target.Instance, observed.Error))
			}
			result.Targets[targetIndex] = observed
		}

		results[selectionIndex] = result
	}

	return results, statusErr
}

// operate runs one mutation batch: preflight opens and checks every
// selected target before any mutation, then apply runs the mutations
// sequentially over the prepared handles. A failing preflight mutates
// nothing anywhere; an apply failure stops the rollout with earlier
// completions retained.
func (g *Group) operate(ctx context.Context, selections []Selection, operation string, desiredHTTP bool, apply func(context.Context, Host, Selection) (host.Result, error)) ([]WorkloadResult, error) {
	results := make([]WorkloadResult, len(selections))
	for index, selection := range selections {
		results[index] = notAttempted(selection)
	}

	handles, preflightErr := g.preflight(ctx, selections, results, desiredHTTP)
	if preflightErr != nil {
		// Nothing was mutated; release every prepared handle once and join
		// the cleanup errors.
		return results, errors.Join(preflightErr, releaseRemaining(handles))
	}

	return g.apply(ctx, selections, results, operation, handles, apply)
}

// preflight opens and checks every selected target before any mutation.
// Each target's open and check run under one bounded context, canceled once
// the check returns: established sessions own their transport lifetime and
// survive that cancellation, so the prepared handle still serves the apply
// pass. Failing targets are recorded as failed with their handles released;
// passing targets keep their prepared handle for apply. Cancellation of the
// operation context stops the remaining checks without mutating anything.
func (g *Group) preflight(ctx context.Context, selections []Selection, results []WorkloadResult, desiredHTTP bool) ([][]Handle, error) {
	handles := make([][]Handle, len(selections))
	var preflightErr error
	for selectionIndex, selection := range selections {
		handles[selectionIndex] = make([]Handle, len(selection.Targets))
		for targetIndex, target := range selection.Targets {
			if err := ctx.Err(); err != nil {
				return handles, errors.Join(preflightErr, fmt.Errorf("preflight %s on %s: %w", selection.Workload, target.Instance, err))
			}

			g.report(host.Progress{Stage: host.StageConnecting, Workload: selection.Workload, Instance: target.Instance})
			checkCtx, cancel := context.WithTimeout(ctx, g.preflightTimeout)
			handle, err := g.provider.Open(checkCtx, target, g.targetReporter(selection.Workload, target.Instance))
			if err != nil {
				cancel()
				results[selectionIndex].Targets[targetIndex].State = TargetFailed
				results[selectionIndex].Targets[targetIndex].Error = fmt.Errorf("connect: %w", err)
				preflightErr = errors.Join(preflightErr, fmt.Errorf("preflight %s on %s: connect: %w", selection.Workload, target.Instance, err))

				continue
			}

			checkErr := handle.Host.Check(checkCtx, selection.Workload, desiredHTTP)
			cancel()
			if checkErr != nil {
				closeErr := handle.Close()
				results[selectionIndex].Targets[targetIndex].State = TargetFailed
				results[selectionIndex].Targets[targetIndex].Error = errors.Join(checkErr, closeErr)
				preflightErr = errors.Join(preflightErr, fmt.Errorf("preflight %s on %s: %w", selection.Workload, target.Instance, errors.Join(checkErr, closeErr)))

				continue
			}
			handles[selectionIndex][targetIndex] = handle
		}
	}

	return handles, preflightErr
}

// apply runs the mutations sequentially over the prepared handles without
// reopening targets, reporting the connecting stage only during preflight.
// Each handle is closed right after its mutation; handles of targets never
// reached are released once.
func (g *Group) apply(ctx context.Context, selections []Selection, results []WorkloadResult, operation string, handles [][]Handle, apply func(context.Context, Host, Selection) (host.Result, error)) ([]WorkloadResult, error) {
	for selectionIndex, selection := range selections {
		for targetIndex, target := range selection.Targets {
			if err := ctx.Err(); err != nil {
				return results, errors.Join(fmt.Errorf("%s %s on %s: %w", operation, selection.Workload, target.Instance, err), releaseRemaining(handles))
			}

			handle := handles[selectionIndex][targetIndex]
			outcome, operationErr := apply(ctx, handle.Host, selection)
			closeErr := handle.Close()
			handles[selectionIndex][targetIndex] = Handle{}
			if operationErr != nil {
				failure := errors.Join(operationErr, closeErr)
				results[selectionIndex].Targets[targetIndex].State = TargetFailed
				results[selectionIndex].Targets[targetIndex].Error = failure
				// The marker survives the join: a lost mutation answer keeps
				// its uncertainty distinct from the failure itself.
				results[selectionIndex].Targets[targetIndex].EffectsUnconfirmed = host.EffectsUnconfirmed(failure)

				return results, errors.Join(fmt.Errorf("%s %s on %s: %w", operation, selection.Workload, target.Instance, failure), releaseRemaining(handles))
			}
			if closeErr != nil {
				// The mutation completed and its outcome is known; keep it
				// and stop with the cleanup failure.
				results[selectionIndex].Targets[targetIndex].State = TargetCompleted
				results[selectionIndex].Targets[targetIndex].Changed = outcome.Changed
				results[selectionIndex].Targets[targetIndex].Container = outcome.Container

				return results, errors.Join(fmt.Errorf("%s %s on %s: close target: %w", operation, selection.Workload, target.Instance, closeErr), releaseRemaining(handles))
			}

			results[selectionIndex].Targets[targetIndex].State = TargetCompleted
			results[selectionIndex].Targets[targetIndex].Changed = outcome.Changed
			results[selectionIndex].Targets[targetIndex].Container = outcome.Container
		}
	}

	return results, nil
}

// releaseRemaining closes every prepared handle exactly once, joining the
// cleanup errors. Already-released handles are zeroed and skipped.
func releaseRemaining(handles [][]Handle) error {
	var closeErr error
	for selectionIndex, selectionHandles := range handles {
		for targetIndex, handle := range selectionHandles {
			if handle.Host == nil {
				continue
			}
			closeErr = errors.Join(closeErr, handle.Close())
			handles[selectionIndex][targetIndex] = Handle{}
		}
	}

	return closeErr
}

// notAttempted preinitializes one selection's mutation result: every
// selected workload and target is present, in order, reporting that nothing
// has run yet.
func notAttempted(selection Selection) WorkloadResult {
	result := WorkloadResult{Workload: selection.Workload, Targets: make([]TargetResult, len(selection.Targets))}
	for index, target := range selection.Targets {
		result.Targets[index] = TargetResult{Target: target, State: TargetNotAttempted}
	}

	return result
}

// targetReporter enriches per-VM host facts with workload and instance
// identity as they pass to the group's reporter.
func (g *Group) targetReporter(workloadName, instance string) host.Reporter {
	return func(fact host.Progress) {
		fact.Workload = workloadName
		fact.Instance = instance
		g.report(fact)
	}
}
