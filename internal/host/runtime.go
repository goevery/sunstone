package host

import (
	"context"
	"slices"

	"github.com/goevery/sunstone/internal/workload"
)

// Runtime performs container and routing operations on one connected VM.
// The deployment module acquires a Runtime per target VM and owns closing it.
type Runtime interface {
	// WorkloadContainers lists the workload's managed containers, including stopped ones.
	WorkloadContainers(ctx context.Context, workload string) ([]Container, error)

	// Matches reports whether the container's effective configuration matches desired.
	Matches(ctx context.Context, current Container, desired ContainerSpec) (bool, error)

	// Logs returns a bounded portion of the container's recent stdout and stderr.
	Logs(ctx context.Context, target Container) (string, error)

	// Pull downloads an image and returns its resolved ID and configuration defaults.
	Pull(ctx context.Context, reference string) (Image, error)

	// Create creates a stopped container from spec, resolving its secret environment values.
	Create(ctx context.Context, spec ContainerSpec) (Container, error)

	// RestartReplacement creates a stopped replacement from the deployed configuration,
	// without pulling images or fetching secrets again.
	RestartReplacement(ctx context.Context, current Container) (Container, error)

	// Start starts the container.
	Start(ctx context.Context, target Container) error

	// Verify checks that the container is running and waits for its Docker health check,
	// if defined, to pass within a bounded time.
	Verify(ctx context.Context, target Container) error

	// Stop gracefully stops the container.
	Stop(ctx context.Context, target Container) error

	// Remove removes the container, forcibly stopping it if it is running.
	Remove(ctx context.Context, target Container) error

	// BackendAddress returns the container address Sunbeam uses to reach containerPort.
	BackendAddress(ctx context.Context, target Container, containerPort uint16) (string, error)

	// Route returns the workload's active route and whether it exists.
	Route(ctx context.Context, workload string) (Route, bool, error)

	// UpdateRoute creates or replaces a route after the backend passes its startup probe,
	// then drains requests to the previous backend.
	UpdateRoute(ctx context.Context, route Route) error

	// DeleteRoute removes the workload's route and drains its admitted requests.
	// A missing route succeeds only when allowMissing is true.
	DeleteRoute(ctx context.Context, workload string, allowMissing bool) error

	// Close releases runtime resources without closing the borrowed session.
	Close() error
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

// Route describes the hostnames and backend Sunbeam routes one HTTP
// workload's traffic through.
type Route struct {
	Workload         string
	Address          string
	ContainerID      string
	Hostnames        []string
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
	Resources         workload.Resources
	HTTP              *workload.HTTPConfig
}

// routeEqual reports whether two routes answer for the same hostnames
// through the same backend.
func routeEqual(left, right Route) bool {
	return left.Workload == right.Workload &&
		left.Address == right.Address &&
		left.ContainerID == right.ContainerID &&
		slices.Equal(left.Hostnames, right.Hostnames) &&
		left.StartupProbePath == right.StartupProbePath
}
