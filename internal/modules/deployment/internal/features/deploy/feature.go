package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const restartPolicy = "unless-stopped"

// Feature coordinates one background workload deployment.
type Feature struct {
	loader    Loader
	connector Connector
	progress  io.Writer
}

// Result describes the observable outcome of a deployment.
type Result struct {
	Workload  string
	Instance  string
	Container string
	Changed   bool
}

// New constructs the deployment feature with all required collaborators.
func New(loader Loader, connector Connector, progress io.Writer) *Feature {
	return &Feature{loader: loader, connector: connector, progress: progress}
}

// Deploy loads and deploys the workload in filename.
func (f *Feature) Deploy(ctx context.Context, filename, serviceAccount string) (Result, error) {
	workload, err := f.loader.Load(filename)
	if err != nil {
		return Result{}, err
	}
	if serviceAccount == "" {
		return Result{}, errors.New("service account to impersonate is required")
	}

	target := Target{
		Project:  workload.GCP.Project,
		Zone:     workload.GCP.Instances[0].Zone,
		Instance: workload.GCP.Instances[0].Name,
	}
	fmt.Fprintf(f.progress, "%s / %s: connecting\n", workload.Name, target.Instance)
	host, err := f.connector.Connect(ctx, target, serviceAccount)
	if err != nil {
		return Result{}, fmt.Errorf("connect to %s: %w", target.Instance, err)
	}
	defer host.Close()

	fmt.Fprintf(f.progress, "%s / %s: pulling %s\n", workload.Name, target.Instance, workload.Container.Image)
	image, err := host.Pull(ctx, workload.Container.Image)
	if err != nil {
		return Result{}, fmt.Errorf("pull image %s: %w", workload.Container.Image, err)
	}
	spec := ContainerSpec{
		Name:          workload.Name,
		Workload:      workload.Name,
		Image:         image,
		Command:       workload.Container.Command,
		Environment:   workload.Container.Environment,
		RestartPolicy: restartPolicy,
	}

	containers, err := host.WorkloadContainers(ctx, workload.Name)
	if err != nil {
		return Result{}, fmt.Errorf("find workload containers: %w", err)
	}
	if len(containers) > 1 {
		return Result{}, fmt.Errorf("workload %s has %d managed containers; refusing ambiguous state", workload.Name, len(containers))
	}
	var previous *Container
	var rollbackPrevious *Container
	if len(containers) == 1 {
		current := containers[0]
		matches, err := host.Matches(ctx, current, spec)
		if err != nil {
			return Result{}, fmt.Errorf("compare current container: %w", err)
		}
		if matches && current.Running && current.Healthy {
			fmt.Fprintf(f.progress, "%s / %s: current\n", workload.Name, target.Instance)
			return Result{Workload: workload.Name, Instance: target.Instance, Container: current.ID}, nil
		}
		if matches && !current.Running {
			if err := host.Start(ctx, current); err != nil {
				return Result{}, fmt.Errorf("start current container: %w", err)
			}
			if err := host.Verify(ctx, current); err != nil {
				return Result{}, fmt.Errorf("verify current container: %w", err)
			}

			return Result{Workload: workload.Name, Instance: target.Instance, Container: current.ID, Changed: true}, nil
		}
		previous = &current
		if current.Running {
			rollbackPrevious = &current
		}
	}

	container, err := host.Create(ctx, spec)
	if err != nil {
		return Result{}, fmt.Errorf("create replacement: %w", err)
	}
	if rollbackPrevious != nil {
		if err := host.Stop(ctx, *rollbackPrevious); err != nil {
			_ = host.Remove(context.WithoutCancel(ctx), container)

			return Result{}, fmt.Errorf("stop current container: %w", err)
		}
	}
	if err := host.Start(ctx, container); err != nil {
		return Result{}, replacementFailure(ctx, host, container, rollbackPrevious, "start", err)
	}
	if err := host.Verify(ctx, container); err != nil {
		return Result{}, replacementFailure(ctx, host, container, rollbackPrevious, "verify", err)
	}
	if previous != nil {
		if err := host.Remove(ctx, *previous); err != nil {
			return Result{}, fmt.Errorf("remove previous container after successful replacement: %w", err)
		}
	}

	fmt.Fprintf(f.progress, "%s / %s: healthy\n", workload.Name, target.Instance)
	return Result{Workload: workload.Name, Instance: target.Instance, Container: container.ID, Changed: true}, nil
}

func replacementFailure(ctx context.Context, host Host, replacement Container, previous *Container, phase string, cause error) error {
	cleanupCtx := context.WithoutCancel(ctx)
	logs, logsErr := host.Logs(cleanupCtx, replacement)
	removeErr := host.Remove(cleanupCtx, replacement)
	var rollbackErr error
	if previous != nil && removeErr == nil {
		rollbackErr = host.Start(cleanupCtx, *previous)
	}

	err := fmt.Errorf("%s replacement: %w", phase, cause)
	if logs != "" {
		err = fmt.Errorf("%w\nreplacement logs:\n%s", err, logs)
	}
	if logsErr != nil {
		err = fmt.Errorf("%w; capture replacement logs: %v", err, logsErr)
	}
	if removeErr != nil {
		err = fmt.Errorf("%w; remove failed replacement: %v", err, removeErr)
	}
	if rollbackErr != nil {
		err = fmt.Errorf("%w; rollback previous container: %v", err, rollbackErr)
	}

	return err
}
