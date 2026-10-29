// Package deployment coordinates one operation across the selected target
// VMs of one or more workloads: it owns ordering, the fail-fast mutation
// policy, best-effort status collection, progress enrichment, and the
// aggregated results. Per-VM container lifecycle, routing, and rollback are
// the host's responsibility, reached through the high-level Host interface
// behind a consumer-owned provider.
package deployment

import (
	"context"

	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/workload"
)

// Host operates one workload on one connected target VM. The group borrows
// each host for one operation; the provider that opened it owns its cleanup.
type Host interface {
	// Check verifies this target can serve a mutation of workloadName
	// without changing anything: Docker access, and Sunbeam control access
	// when the workload needs routing on this target — desiredHTTP says
	// whether the desired configuration is HTTP; name-only operations pass
	// false and follow deployed metadata instead.
	Check(ctx context.Context, workloadName string, desiredHTTP bool) error
	Deploy(ctx context.Context, definition workload.Definition) (host.Result, error)
	Status(ctx context.Context, workloadName string) (host.Observation, error)
	Restart(ctx context.Context, workloadName string) (host.Result, error)
	Remove(ctx context.Context, workloadName string) (host.Result, error)
}

// The production per-VM host satisfies the group's Host interface.
var _ Host = (*host.Host)(nil)

// Target identifies one Compute Engine VM a workload is configured on.
type Target struct {
	Project  string
	Zone     string
	Instance string
}

// Selection names one workload and the targets an operation runs on. The
// name-only operations (status, restart, remove) need no configuration.
type Selection struct {
	Workload string
	Targets  []Target
}

// SelectionOf derives the selection of one validated workload definition.
func SelectionOf(definition workload.Definition) Selection {
	targets := make([]Target, len(definition.GCP.Instances))
	for index, instance := range definition.GCP.Instances {
		targets[index] = Target{Project: definition.GCP.Project, Zone: instance.Zone, Instance: instance.Name}
	}

	return Selection{Workload: definition.Name, Targets: targets}
}

// Provider opens one target VM for an operation, yielding the high-level
// host together with the mandatory cleanup of the operation-scoped resources
// behind it. The received reporter is the target's enriched progress
// reporter; providers that build hosts bind it to the host they return.
type Provider interface {
	Open(ctx context.Context, target Target, report host.Reporter) (Handle, error)
}

// Handle couples an opened target host with the mandatory cleanup that
// releases the operation-scoped runtime resources behind it: the HTTP and
// Docker clients and the operation's fetched or enrolled secret state.
// Close is called exactly once when the operation on the target ends,
// whether the operation succeeded or failed. It never touches the
// transport underneath, which is command-owned and outlives the handle.
type Handle struct {
	Host  Host
	Close func() error
}

// Mutation target outcome states.
const (
	// TargetCompleted means the operation finished on this target.
	TargetCompleted = "completed"
	// TargetFailed means the operation failed on this target.
	TargetFailed = "failed"
	// TargetNotAttempted means the operation never started on this target.
	TargetNotAttempted = "not-attempted"
)

// StateUnreachable classifies a status target that could not be observed.
const StateUnreachable = "unreachable"

// WorkloadResult reports one workload's mutation outcome on every selected
// target, in selection order.
type WorkloadResult struct {
	Workload string
	Targets  []TargetResult
}

// TargetResult reports one selected target's mutation outcome. A completed
// target carries its container outcome; a failed target carries the error;
// a not-attempted target carries neither. EffectsUnconfirmed is never
// another execution state: it separately reports that the remote's
// acceptance of the failed mutation — or the final state after it — could
// not be established and the target needs manual inspection before
// retrying.
type TargetResult struct {
	Target             Target
	State              string
	Changed            bool
	Container          string
	Error              error
	EffectsUnconfirmed bool
}

// WorkloadStatus reports one workload's observations on every selected
// target, in selection order.
type WorkloadStatus struct {
	Workload string
	Targets  []TargetStatus
}

// TargetStatus reports one target's observation and any error that limited
// it. A failed observation retains whatever the host reported.
type TargetStatus struct {
	Target      Target
	Observation host.Observation
	Error       error
}
