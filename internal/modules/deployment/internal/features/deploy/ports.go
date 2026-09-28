package deploy

import "context"

// Loader obtains one validated workload definition.
type Loader interface {
	Load(string) (Workload, error)
}

// Connector establishes access to a target VM's container runtime.
type Connector interface {
	Connect(context.Context, Target, string) (Host, error)
}

// Host exposes the container operations needed by a deployment.
type Host interface {
	Close() error
	Pull(context.Context, string) (Image, error)
	WorkloadContainers(context.Context, string) ([]Container, error)
	Matches(context.Context, Container, ContainerSpec) (bool, error)
	Create(context.Context, ContainerSpec) (Container, error)
	Start(context.Context, Container) error
	Stop(context.Context, Container) error
	Verify(context.Context, Container) error
	Logs(context.Context, Container) (string, error)
	Remove(context.Context, Container) error
}

// Workload is the desired background workload loaded from configuration.
type Workload struct {
	Name      string
	GCP       GCP
	Container ContainerConfig
}

// GCP identifies the project and target instances for a workload.
type GCP struct {
	Project   string
	Instances []Instance
}

// Instance identifies one configured Compute Engine VM.
type Instance struct {
	Zone string
	Name string
}

// ContainerConfig contains the supported container configuration.
type ContainerConfig struct {
	Image       string
	Command     []string
	Environment map[string]string
}

// Target identifies one Compute Engine VM.
type Target struct {
	Project  string
	Zone     string
	Instance string
}

// Image is a pulled, content-addressed container image.
type Image struct {
	ID                 string
	DefaultCommand     []string
	DefaultEnvironment []string
}

// Container identifies a Sunstone-managed container and its observed state.
type Container struct {
	ID      string
	Name    string
	Running bool
	Healthy bool
}

// ContainerSpec is the desired runtime configuration.
type ContainerSpec struct {
	Name          string
	Workload      string
	Image         Image
	Command       []string
	Environment   map[string]string
	RestartPolicy string
}
