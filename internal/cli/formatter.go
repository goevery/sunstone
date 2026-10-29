// Package cli renders operation progress and results as human-readable
// command-line text. All status, mutation-summary, and progress formatting
// lives here; commands only load, wire, run, and render.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/host"
)

// Formatter renders progress facts, status observations, and mutation
// results as one text line per fact. A write failure never interrupts the
// operation: the first failure is retained, later lines are dropped, and
// Err reports it after the operation completes.
type Formatter struct {
	writer io.Writer
	err    error
}

// NewFormatter constructs a progress formatter writing to writer, which is
// mandatory.
func NewFormatter(writer io.Writer) (*Formatter, error) {
	if writer == nil {
		return nil, errors.New("writer is required")
	}

	return &Formatter{writer: writer}, nil
}

// Report renders one progress fact.
func (f *Formatter) Report(progress host.Progress) {
	f.line("%s / %s: %s\n", progress.Workload, progress.Instance, render(progress))
}

// WriteStatus renders every workload target's observed state with its
// containers, its active route, and any observation error. Partial
// observations keep their detail; no deployed metadata or secret payload
// is ever rendered.
func (f *Formatter) WriteStatus(statuses []deployment.WorkloadStatus) {
	for _, workloadStatus := range statuses {
		for _, target := range workloadStatus.Targets {
			f.line("%s / %s (%s): %s\n", workloadStatus.Workload, target.Target.Instance, target.Target.Zone, target.Observation.State)
			for _, container := range target.Observation.Containers {
				f.line("  container: %s name=%s image=%s running=%t healthy=%t\n", container.ID, container.Name, container.Image, container.Running, container.Healthy)
			}

			if target.Observation.Route != nil {
				f.line("  route: %s -> %s\n", target.Observation.Route.Address, target.Observation.Route.ContainerID)
			}

			if target.Error != nil {
				f.line("  error: %s\n", target.Error)
			}
		}
	}
}

// WriteResults renders every selected workload and target's mutation
// outcome — completed with its change outcome, failed with its error, or
// not attempted — and warns explicitly when a failure's remote effects are
// unconfirmed. Results render after both successful and failed operations.
func (f *Formatter) WriteResults(results []deployment.WorkloadResult) {
	for _, workloadResult := range results {
		for _, target := range workloadResult.Targets {
			f.line("%s / %s (%s): %s\n", workloadResult.Workload, target.Target.Instance, target.Target.Zone, describeOutcome(target))
			if target.State == deployment.TargetCompleted && target.Container != "" {
				f.line("  container: %s\n", target.Container)
			}
			if target.State == deployment.TargetFailed {
				f.line("  error: %s\n", target.Error)
			}
			if target.EffectsUnconfirmed {
				f.line("  warning: remote changes may have occurred; inspect before retrying\n")
			}
		}
	}
}

// describeOutcome renders one target's mutation state, including whether a
// completed target actually changed.
func describeOutcome(target deployment.TargetResult) string {
	switch target.State {
	case deployment.TargetCompleted:
		if target.Changed {
			return "completed (changed)"
		}

		return "completed (unchanged)"
	case deployment.TargetFailed:
		return "failed"
	case deployment.TargetNotAttempted:
		return "not attempted"
	}

	return target.State
}

// Err returns the first output write failure, if any.
func (f *Formatter) Err() error {
	return f.err
}

// line renders one text line unless an earlier write already failed.
func (f *Formatter) line(format string, arguments ...any) {
	if f.err != nil {
		return
	}
	_, f.err = fmt.Fprintf(f.writer, format, arguments...)
}

func render(progress host.Progress) string {
	if progress.Stage == host.StagePulling && progress.Image != "" {
		return string(host.StagePulling) + " " + progress.Image
	}

	return string(progress.Stage)
}
