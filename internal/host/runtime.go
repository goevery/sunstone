package host

import (
	"context"

	"github.com/goevery/sunstone/internal/workload"
)

// Runtime performs container and routing operations on one connected VM.
// The deployment module acquires a Runtime per target VM and owns closing it.
type Runtime interface {
	Close() error
	Pull(context.Context, string) (Image, error)
	WorkloadContainers(context.Context, string) ([]Container, error)
	Matches(context.Context, Container, ContainerSpec) (bool, error)
	Create(context.Context, ContainerSpec) (Container, error)
	RestartReplacement(ctx context.Context, current Container) (Container, error)
	Start(context.Context, Container) error
	BackendAddress(context.Context, Container, uint16) (string, error)
	Route(context.Context, string) (Route, bool, error)
	UpdateRoute(context.Context, Route) error
	DeleteRoute(context.Context, string, bool) error
	Stop(context.Context, Container) error
	Verify(context.Context, Container) error
	Logs(context.Context, Container) (string, error)
	Remove(context.Context, Container) error
}

// Image is a pulled, content-addressed container image.
type Image struct {
	ID                 string
	DefaultCommand     []string
	DefaultEnvironment []string
}

// Container identifies a Sunstone-managed container and its observed state.
// Metadata is the deployed metadata decoded from the container's labels;
// nil means the deployed metadata is missing, malformed, or contradictory.
type Container struct {
	ID       string
	Name     string
	Image    string
	Running  bool
	Healthy  bool
	Metadata *DeployedMetadata
}

// Route describes the backend Sunbeam should receive traffic through.
type Route struct {
	Workload         string
	Address          string
	ContainerID      string
	StartupProbePath string
}

// ContainerSpec is the desired runtime configuration.
type ContainerSpec struct {
	Name              string
	Workload          string
	Image             Image
	Command           []string
	Environment       map[string]string
	SecretEnvironment map[string]string
	RestartPolicy     string
	HTTP              *workload.HTTPConfig
}
