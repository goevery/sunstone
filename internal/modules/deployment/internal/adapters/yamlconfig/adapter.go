package yamlconfig

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"go.yaml.in/yaml/v3"
)

// Adapter loads the initial single-document YAML workload format.
type Adapter struct{}

// New constructs a YAML workload adapter.
func New() *Adapter {
	return &Adapter{}
}

// Load decodes and validates one workload from filename.
func (*Adapter) Load(filename string) (deploy.Workload, error) {
	file, err := os.Open(filename)
	if err != nil {
		return deploy.Workload{}, fmt.Errorf("open workload: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var document workloadDocument
	if err := decoder.Decode(&document); err != nil {
		return deploy.Workload{}, fmt.Errorf("decode workload: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return deploy.Workload{}, errors.New("workload file must contain exactly one YAML document")
		}

		return deploy.Workload{}, fmt.Errorf("decode workload: %w", err)
	}
	if document.Name == "" || document.GCP.Project == "" || document.Container.Image == "" {
		return deploy.Workload{}, errors.New("workload name, gcp.project, and container.image are required")
	}
	if len(document.GCP.Instances) == 0 {
		return deploy.Workload{}, errors.New("at least one GCP instance is required")
	}
	instances := make([]deploy.Instance, len(document.GCP.Instances))
	seen := make(map[deploy.Instance]struct{}, len(document.GCP.Instances))
	for index, documentInstance := range document.GCP.Instances {
		if documentInstance.Name == "" || documentInstance.Zone == "" {
			return deploy.Workload{}, errors.New("every GCP instance must have a name and zone")
		}
		instance := deploy.Instance{Zone: documentInstance.Zone, Name: documentInstance.Name}
		if _, exists := seen[instance]; exists {
			return deploy.Workload{}, fmt.Errorf("duplicate GCP instance %s/%s", instance.Zone, instance.Name)
		}
		seen[instance] = struct{}{}
		instances[index] = instance
	}

	return deploy.Workload{
		Name: document.Name,
		GCP: deploy.GCP{
			Project:   document.GCP.Project,
			Instances: instances,
		},
		Container: deploy.ContainerConfig{
			Image:       document.Container.Image,
			Command:     document.Container.Command,
			Environment: map[string]string(document.Container.Environment),
		},
	}, nil
}

type workloadDocument struct {
	Name      string            `yaml:"name"`
	GCP       gcpDocument       `yaml:"gcp"`
	Container containerDocument `yaml:"container"`
}

type gcpDocument struct {
	Project   string             `yaml:"project"`
	Instances []instanceDocument `yaml:"instances"`
}

type instanceDocument struct {
	Zone string `yaml:"zone"`
	Name string `yaml:"name"`
}

type containerDocument struct {
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command"`
	Environment strictEnvironment `yaml:"env"`
}

type strictEnvironment map[string]string

func (environment *strictEnvironment) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("container.env must be a mapping of string values")
	}
	result := make(strictEnvironment, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		value := node.Content[index+1]
		if key.Tag != "!!str" || value.Tag != "!!str" {
			return fmt.Errorf("container.env key %q must have a string value", key.Value)
		}
		result[key.Value] = value.Value
	}
	*environment = result
	return nil
}
