package deployment

import (
	"context"
	"io"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/googlehost"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/yamlconfig"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

// Module deploys and operates workloads on their configured VMs.
type Module interface {
	Deploy(context.Context, Request) (Result, error)
	Status(context.Context, WorkloadRequest) (StatusResult, error)
	Restart(context.Context, WorkloadRequest) (OperationResult, error)
	Remove(context.Context, WorkloadRequest) (OperationResult, error)
}

// Request selects a workload definition and the service account used to deploy it.
type Request struct {
	Filename                  string
	ImpersonateServiceAccount string
}

// WorkloadRequest selects workload files or directories and the operator identity.
type WorkloadRequest struct {
	Filenames                 []string
	ImpersonateServiceAccount string
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

// StatusResult contains observations for all selected workloads and targets.
type StatusResult struct {
	Workloads []WorkloadStatus
}

// WorkloadStatus contains observations for one workload.
type WorkloadStatus struct {
	Workload  string
	Instances []InstanceStatus
}

// InstanceStatus is the observed state of one workload target.
type InstanceStatus struct {
	Zone       string
	Instance   string
	State      string
	Containers []ContainerStatus
	Route      *RouteStatus
	Error      string
}

// ContainerStatus is the observed state of one managed container.
type ContainerStatus struct {
	ID      string
	Name    string
	Image   string
	Running bool
	Healthy bool
}

// RouteStatus is the active Sunbeam route observed on one VM.
type RouteStatus struct {
	Address     string
	ContainerID string
}

// OperationResult contains completed work for restart or removal.
type OperationResult struct {
	Workloads []Result
}

type module struct {
	feature *deploy.Feature
}

// New constructs the production workload module.
func New(progress io.Writer) (Module, error) {
	connector, err := googlehost.New()
	if err != nil {
		return nil, err
	}

	return newModule(yamlconfig.New(), connector, progress), nil
}

func newModule(loader deploy.Loader, connector deploy.Connector, progress io.Writer) Module {
	return &module{feature: deploy.New(loader, connector, progress)}
}

func (m *module) Deploy(ctx context.Context, request Request) (Result, error) {
	result, err := m.feature.Deploy(ctx, request.Filename, request.ImpersonateServiceAccount)
	return resultFromFeature(result), err
}

func (m *module) Status(ctx context.Context, request WorkloadRequest) (StatusResult, error) {
	observed, err := m.feature.Status(ctx, request.Filenames, request.ImpersonateServiceAccount)
	result := StatusResult{Workloads: make([]WorkloadStatus, len(observed))}
	for workloadIndex, workload := range observed {
		instances := make([]InstanceStatus, len(workload.Instances))
		for instanceIndex, instance := range workload.Instances {
			containers := make([]ContainerStatus, len(instance.Containers))
			for containerIndex, container := range instance.Containers {
				containers[containerIndex] = ContainerStatus{ID: container.ID, Name: container.Name, Image: container.Image, Running: container.Running, Healthy: container.Healthy}
			}
			var route *RouteStatus
			if instance.Route != nil {
				route = &RouteStatus{Address: instance.Route.Address, ContainerID: instance.Route.ContainerID}
			}
			instances[instanceIndex] = InstanceStatus{Zone: instance.Zone, Instance: instance.Instance, State: instance.State, Containers: containers, Route: route, Error: instance.Error}
		}
		result.Workloads[workloadIndex] = WorkloadStatus{Workload: workload.Workload, Instances: instances}
	}
	return result, err
}

func (m *module) Restart(ctx context.Context, request WorkloadRequest) (OperationResult, error) {
	results, err := m.feature.Restart(ctx, request.Filenames, request.ImpersonateServiceAccount)
	return operationResult(results), err
}

func (m *module) Remove(ctx context.Context, request WorkloadRequest) (OperationResult, error) {
	results, err := m.feature.RemoveWorkloads(ctx, request.Filenames, request.ImpersonateServiceAccount)
	return operationResult(results), err
}

func operationResult(results []deploy.OperateResult) OperationResult {
	converted := make([]Result, len(results))
	for index, result := range results {
		converted[index] = resultFromFeature(deploy.Result{Workload: result.Workload, Instances: result.Instances})
	}
	return OperationResult{Workloads: converted}
}

func resultFromFeature(result deploy.Result) Result {
	instances := make([]InstanceResult, len(result.Instances))
	for index, instance := range result.Instances {
		instances[index] = InstanceResult(instance)
	}
	return Result{Workload: result.Workload, Instances: instances}
}
