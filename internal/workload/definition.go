// Package workload defines the canonical workload definition types and
// loads them from the single-document YAML format.
package workload

// Definition is the desired container workload loaded from configuration.
type Definition struct {
	Name      string
	GCP       GCP
	Container ContainerConfig
	HTTP      *HTTPConfig
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
	// SecretEnvironment contains normalized version references, never payloads.
	SecretEnvironment map[string]string
}

// HTTPConfig selects HTTP deployment and describes its startup gate.
type HTTPConfig struct {
	ContainerPort    uint16
	StartupProbePath string
}
