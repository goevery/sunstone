package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
)

const restartPolicy = "unless-stopped"

// Feature coordinates one workload deployment across its target VMs.
type Feature struct {
	loader    Loader
	connector Connector
	progress  io.Writer
}

// Result describes the completed instances in a workload deployment.
type Result struct {
	Workload  string
	Instances []InstanceResult
}

// InstanceResult describes the observable outcome on one target VM.
type InstanceResult struct {
	Zone      string
	Instance  string
	Container string
	Changed   bool
}

// New constructs the deployment feature with all required collaborators.
func New(loader Loader, connector Connector, progress io.Writer) *Feature {
	return &Feature{loader: loader, connector: connector, progress: progress}
}

// Deploy loads and sequentially deploys the workload in filename.
func (f *Feature) Deploy(ctx context.Context, filename, serviceAccount string) (Result, error) {
	workload, err := f.loader.Load(filename)
	if err != nil {
		return Result{}, err
	}
	if serviceAccount == "" {
		return Result{}, errors.New("service account to impersonate is required")
	}

	result := Result{Workload: workload.Name}
	for _, instance := range workload.GCP.Instances {
		target := Target{
			Project:  workload.GCP.Project,
			Zone:     instance.Zone,
			Instance: instance.Name,
		}
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("deploy to %s: %w", target.Instance, err)
		}

		instanceResult, err := f.deployToInstance(ctx, workload, target, serviceAccount)
		if err != nil {
			return result, fmt.Errorf("deploy to %s: %w", target.Instance, err)
		}
		result.Instances = append(result.Instances, instanceResult)
	}

	return result, nil
}

func (f *Feature) deployToInstance(ctx context.Context, workload Workload, target Target, serviceAccount string) (result InstanceResult, resultErr error) {
	fmt.Fprintf(f.progress, "%s / %s: connecting\n", workload.Name, target.Instance)
	host, err := f.connector.Connect(ctx, target, serviceAccount)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("connect: %w", err)
	}
	defer func() {
		closeErr := host.Close()
		if closeErr == nil {
			return
		}
		if resultErr != nil {
			resultErr = fmt.Errorf("%w; close connection: %v", resultErr, closeErr)
			return
		}

		result = InstanceResult{}
		resultErr = fmt.Errorf("close connection: %w", closeErr)
	}()

	fmt.Fprintf(f.progress, "%s / %s: pulling %s\n", workload.Name, target.Instance, workload.Container.Image)
	image, err := host.Pull(ctx, workload.Container.Image)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("pull image %s: %w", workload.Container.Image, err)
	}
	spec := ContainerSpec{
		Name:          workload.Name,
		Workload:      workload.Name,
		Image:         image,
		Command:       workload.Container.Command,
		Environment:   workload.Container.Environment,
		RestartPolicy: restartPolicy,
		HTTP:          workload.HTTP,
	}

	containers, err := host.WorkloadContainers(ctx, workload.Name)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("find workload containers: %w", err)
	}
	if len(containers) > 1 {
		return InstanceResult{}, fmt.Errorf("workload %s has %d managed containers; refusing ambiguous state", workload.Name, len(containers))
	}
	if workload.HTTP != nil {
		return f.deployHTTP(ctx, host, workload, target, spec, containers)
	}

	var previous *Container
	var rollbackPrevious *Container
	if len(containers) == 1 {
		current := containers[0]
		matches, err := host.Matches(ctx, current, spec)
		if err != nil {
			return InstanceResult{}, fmt.Errorf("compare current container: %w", err)
		}
		if matches && current.Running && current.Healthy {
			fmt.Fprintf(f.progress, "%s / %s: current\n", workload.Name, target.Instance)
			return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: current.ID}, nil
		}
		if matches && !current.Running {
			if err := host.Start(ctx, current); err != nil {
				return InstanceResult{}, fmt.Errorf("start current container: %w", err)
			}
			if err := host.Verify(ctx, current); err != nil {
				return InstanceResult{}, fmt.Errorf("verify current container: %w", err)
			}

			return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: current.ID, Changed: true}, nil
		}
		previous = &current
		if current.Running {
			rollbackPrevious = &current
		}
	}

	container, err := host.Create(ctx, spec)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("create replacement: %w", err)
	}
	if rollbackPrevious != nil {
		if err := host.Stop(ctx, *rollbackPrevious); err != nil {
			_ = host.Remove(context.WithoutCancel(ctx), container)

			return InstanceResult{}, fmt.Errorf("stop current container: %w", err)
		}
	}
	if err := host.Start(ctx, container); err != nil {
		return InstanceResult{}, replacementFailure(ctx, host, container, rollbackPrevious, "start", err)
	}
	if err := host.Verify(ctx, container); err != nil {
		return InstanceResult{}, replacementFailure(ctx, host, container, rollbackPrevious, "verify", err)
	}
	if previous != nil {
		if err := host.Remove(ctx, *previous); err != nil {
			return InstanceResult{}, fmt.Errorf("remove previous container after successful replacement: %w", err)
		}
	}

	fmt.Fprintf(f.progress, "%s / %s: healthy\n", workload.Name, target.Instance)
	return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: container.ID, Changed: true}, nil
}

func (f *Feature) deployHTTP(ctx context.Context, host Host, workload Workload, target Target, spec ContainerSpec, containers []Container) (InstanceResult, error) {
	var previous *Container
	if len(containers) == 1 {
		current := containers[0]
		matches, err := host.Matches(ctx, current, spec)
		if err != nil {
			return InstanceResult{}, fmt.Errorf("compare current container: %w", err)
		}
		if matches && current.Running {
			address, err := host.BackendAddress(ctx, current, workload.HTTP.ContainerPort)
			if err != nil {
				return InstanceResult{}, fmt.Errorf("find current HTTP backend: %w", err)
			}
			desiredRoute := Route{Workload: workload.Name, Address: address, ContainerID: current.ID, StartupProbePath: workload.HTTP.StartupProbePath}
			activeRoute, found, err := host.Route(ctx, workload.Name)
			if err != nil {
				return InstanceResult{}, fmt.Errorf("get current route: %w", err)
			}
			if found && activeRoute == desiredRoute {
				fmt.Fprintf(f.progress, "%s / %s: current\n", workload.Name, target.Instance)
				return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: current.ID}, nil
			}
			fmt.Fprintf(f.progress, "%s / %s: routing\n", workload.Name, target.Instance)
			if err := host.UpdateRoute(ctx, desiredRoute); err != nil {
				return InstanceResult{}, fmt.Errorf("update route: %w", err)
			}
			fmt.Fprintf(f.progress, "%s / %s: drained\n", workload.Name, target.Instance)
			fmt.Fprintf(f.progress, "%s / %s: healthy\n", workload.Name, target.Instance)
			return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: current.ID, Changed: true}, nil
		}
		previous = &current
	}

	replacement, err := host.Create(ctx, spec)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("create replacement: %w", err)
	}
	fmt.Fprintf(f.progress, "%s / %s: starting\n", workload.Name, target.Instance)
	if err := host.Start(ctx, replacement); err != nil {
		return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "start", err)
	}
	address, err := host.BackendAddress(ctx, replacement, workload.HTTP.ContainerPort)
	if err != nil {
		return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "find HTTP backend", err)
	}
	route := Route{Workload: workload.Name, Address: address, ContainerID: replacement.ID, StartupProbePath: workload.HTTP.StartupProbePath}
	fmt.Fprintf(f.progress, "%s / %s: routing\n", workload.Name, target.Instance)
	if err := host.UpdateRoute(ctx, route); err != nil {
		return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "update route", err)
	}
	fmt.Fprintf(f.progress, "%s / %s: drained\n", workload.Name, target.Instance)
	if previous != nil {
		if err := host.Remove(ctx, *previous); err != nil {
			return InstanceResult{}, fmt.Errorf("remove previous container after draining: %w", err)
		}
	}

	fmt.Fprintf(f.progress, "%s / %s: healthy\n", workload.Name, target.Instance)
	return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: replacement.ID, Changed: true}, nil
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
