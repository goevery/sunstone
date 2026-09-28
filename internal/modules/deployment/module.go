package deployment

import (
	"context"
	"io"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/googlehost"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/yamlconfig"
	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
)

// Module deploys background workloads to their configured VM.
type Module interface {
	Deploy(context.Context, Request) (Result, error)
}

// Request selects a workload definition and the service account used to deploy it.
type Request struct {
	Filename                  string
	ImpersonateServiceAccount string
}

// Result describes the observable outcome of a deployment.
type Result struct {
	Workload  string
	Instance  string
	Container string
	Changed   bool
}

type module struct {
	feature *deploy.Feature
}

// New constructs the production deployment module.
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
	return Result(result), err
}
