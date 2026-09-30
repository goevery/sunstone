package deploy

import (
	"context"
	"errors"
	"fmt"
)

// WorkloadStatus describes observations for one configured workload.
type WorkloadStatus struct {
	Workload  string
	Instances []InstanceStatus
}

// InstanceStatus describes one workload target without changing it.
type InstanceStatus struct {
	Zone       string
	Instance   string
	State      string
	Containers []Container
	Route      *Route
	Error      string
}

// OperateResult describes completed restart or removal work.
type OperateResult struct {
	Workload  string
	Instances []InstanceResult
}

// Status observes all configured targets, retaining per-target failures.
func (f *Feature) Status(ctx context.Context, filenames []string, serviceAccount string) ([]WorkloadStatus, error) {
	workloads, err := f.loadOperationWorkloads(filenames, serviceAccount)
	if err != nil {
		return nil, err
	}

	results := make([]WorkloadStatus, 0, len(workloads))
	var statusErr error
	for _, workload := range workloads {
		result := WorkloadStatus{Workload: workload.Name}
		for _, instance := range workload.GCP.Instances {
			target := Target{Project: workload.GCP.Project, Zone: instance.Zone, Instance: instance.Name}
			observed := InstanceStatus{Zone: instance.Zone, Instance: instance.Name}
			if err := ctx.Err(); err != nil {
				observed.State = "unreachable"
				observed.Error = err.Error()
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: %w", workload.Name, instance.Name, err))
				result.Instances = append(result.Instances, observed)
				continue
			}

			host, err := f.connector.Connect(ctx, target, serviceAccount)
			if err != nil {
				observed.State = "unreachable"
				observed.Error = err.Error()
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: connect: %w", workload.Name, instance.Name, err))
				result.Instances = append(result.Instances, observed)
				continue
			}
			containers, observeErr := host.WorkloadContainers(ctx, workload.Name)
			observed.Containers = containers
			if observeErr == nil && workload.HTTP != nil {
				route, found, routeErr := host.Route(ctx, workload.Name)
				observeErr = routeErr
				if found {
					observed.Route = &route
				}
			}
			closeErr := host.Close()
			observeErr = errors.Join(observeErr, closeErr)
			if observeErr != nil {
				observed.State = "unreachable"
				observed.Error = observeErr.Error()
				statusErr = errors.Join(statusErr, fmt.Errorf("status %s on %s: %w", workload.Name, instance.Name, observeErr))
			} else {
				observed.State = statusState(workload, containers, observed.Route)
			}
			result.Instances = append(result.Instances, observed)
		}
		results = append(results, result)
	}
	return results, statusErr
}

// Restart restarts configured workloads sequentially and stops on the first failure.
func (f *Feature) Restart(ctx context.Context, filenames []string, serviceAccount string) ([]OperateResult, error) {
	workloads, err := f.loadOperationWorkloads(filenames, serviceAccount)
	if err != nil {
		return nil, err
	}
	return f.operate(ctx, workloads, serviceAccount, "restart", f.restartOnHost)
}

// Remove removes configured workloads sequentially and stops on the first failure.
func (f *Feature) RemoveWorkloads(ctx context.Context, filenames []string, serviceAccount string) ([]OperateResult, error) {
	workloads, err := f.loadOperationWorkloads(filenames, serviceAccount)
	if err != nil {
		return nil, err
	}
	return f.operate(ctx, workloads, serviceAccount, "remove", f.removeFromHost)
}

func (f *Feature) loadOperationWorkloads(filenames []string, serviceAccount string) ([]Workload, error) {
	if serviceAccount == "" {
		return nil, errors.New("service account to impersonate is required")
	}
	return f.loader.LoadAll(filenames)
}

func (f *Feature) operate(ctx context.Context, workloads []Workload, serviceAccount, operation string, apply func(context.Context, Host, Workload, Target) (InstanceResult, error)) ([]OperateResult, error) {
	results := make([]OperateResult, 0, len(workloads))
	for _, workload := range workloads {
		result := OperateResult{Workload: workload.Name}
		for _, instance := range workload.GCP.Instances {
			target := Target{Project: workload.GCP.Project, Zone: instance.Zone, Instance: instance.Name}
			if err := ctx.Err(); err != nil {
				results = append(results, result)
				return results, fmt.Errorf("%s %s on %s: %w", operation, workload.Name, instance.Name, err)
			}
			fmt.Fprintf(f.progress, "%s / %s: connecting\n", workload.Name, instance.Name)
			host, err := f.connector.Connect(ctx, target, serviceAccount)
			if err != nil {
				results = append(results, result)
				return results, fmt.Errorf("%s %s on %s: connect: %w", operation, workload.Name, instance.Name, err)
			}
			instanceResult, operationErr := apply(ctx, host, workload, target)
			closeErr := host.Close()
			if operationErr != nil || closeErr != nil {
				results = append(results, result)
				return results, fmt.Errorf("%s %s on %s: %w", operation, workload.Name, instance.Name, errors.Join(operationErr, closeErr))
			}
			result.Instances = append(result.Instances, instanceResult)
		}
		results = append(results, result)
	}
	return results, nil
}

func (f *Feature) restartOnHost(ctx context.Context, host Host, workload Workload, target Target) (InstanceResult, error) {
	containers, err := host.WorkloadContainers(ctx, workload.Name)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("find workload containers: %w", err)
	}
	if len(containers) == 0 {
		return InstanceResult{}, errors.New("workload is missing")
	}
	if len(containers) > 1 {
		return InstanceResult{}, fmt.Errorf("workload has %d managed containers; refusing ambiguous state", len(containers))
	}
	current := containers[0]
	if workload.HTTP != nil {
		image, err := host.Pull(ctx, workload.Container.Image)
		if err != nil {
			return InstanceResult{}, fmt.Errorf("pull image %s: %w", workload.Container.Image, err)
		}
		spec := ContainerSpec{Name: workload.Name, Workload: workload.Name, Image: image, Command: workload.Container.Command, Environment: workload.Container.Environment, RestartPolicy: restartPolicy, HTTP: workload.HTTP}
		replacement, err := host.Create(ctx, spec)
		if err != nil {
			return InstanceResult{}, fmt.Errorf("create replacement: %w", err)
		}
		if err := host.Start(ctx, replacement); err != nil {
			return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "start", err)
		}
		address, err := host.BackendAddress(ctx, replacement, workload.HTTP.ContainerPort)
		if err != nil {
			return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "find HTTP backend", err)
		}
		route := Route{Workload: workload.Name, Address: address, ContainerID: replacement.ID, StartupProbePath: workload.HTTP.StartupProbePath}
		if err := host.UpdateRoute(ctx, route); err != nil {
			return InstanceResult{}, replacementFailure(ctx, host, replacement, nil, "update route", err)
		}
		if err := host.Remove(ctx, current); err != nil {
			return InstanceResult{}, fmt.Errorf("remove previous container after draining: %w", err)
		}
		fmt.Fprintf(f.progress, "%s / %s: restarted\n", workload.Name, target.Instance)
		return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: replacement.ID, Changed: true}, nil
	}

	if current.Running {
		if err := host.Stop(ctx, current); err != nil {
			return InstanceResult{}, fmt.Errorf("stop container: %w", err)
		}
	}
	if err := host.Start(ctx, current); err != nil {
		return InstanceResult{}, fmt.Errorf("start container: %w", err)
	}
	if err := host.Verify(ctx, current); err != nil {
		return InstanceResult{}, fmt.Errorf("verify container: %w", err)
	}
	fmt.Fprintf(f.progress, "%s / %s: restarted\n", workload.Name, target.Instance)
	return InstanceResult{Zone: target.Zone, Instance: target.Instance, Container: current.ID, Changed: true}, nil
}

func (f *Feature) removeFromHost(ctx context.Context, host Host, workload Workload, target Target) (InstanceResult, error) {
	containers, err := host.WorkloadContainers(ctx, workload.Name)
	if err != nil {
		return InstanceResult{}, fmt.Errorf("find workload containers: %w", err)
	}
	if workload.HTTP != nil {
		if err := host.DeleteRoute(ctx, workload.Name, true); err != nil {
			return InstanceResult{}, fmt.Errorf("delete route: %w", err)
		}
	}
	for _, container := range containers {
		if workload.HTTP == nil && container.Running {
			if err := host.Stop(ctx, container); err != nil {
				return InstanceResult{}, fmt.Errorf("stop container %s: %w", container.ID, err)
			}
		}
		if err := host.Remove(ctx, container); err != nil {
			return InstanceResult{}, fmt.Errorf("remove container %s: %w", container.ID, err)
		}
	}
	fmt.Fprintf(f.progress, "%s / %s: removed\n", workload.Name, target.Instance)
	return InstanceResult{Zone: target.Zone, Instance: target.Instance, Changed: len(containers) != 0}, nil
}

func statusState(workload Workload, containers []Container, route *Route) string {
	if len(containers) == 0 {
		return "missing"
	}
	if len(containers) > 1 {
		return "ambiguous"
	}
	container := containers[0]
	if !container.Running {
		return "stopped"
	}
	if !container.Healthy {
		return "unhealthy"
	}
	if workload.HTTP == nil {
		return "current"
	}
	if route == nil {
		return "route-missing"
	}
	if route.ContainerID != container.ID {
		return "route-mismatch"
	}
	return "current"
}
