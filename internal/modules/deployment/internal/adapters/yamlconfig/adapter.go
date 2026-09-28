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
	if len(document.GCP.Instances) != 1 || document.GCP.Instances[0].Name == "" || document.GCP.Instances[0].Zone == "" {
		return deploy.Workload{}, errors.New("exactly one GCP instance with name and zone is required")
	}
	instance := document.GCP.Instances[0]
	return deploy.Workload{
		Name: document.Name,
		GCP: deploy.GCP{
			Project:   document.GCP.Project,
			Instances: []deploy.Instance{{Zone: instance.Zone, Name: instance.Name}},
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
