package yamlconfig

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/features/deploy"
	"go.yaml.in/yaml/v3"
)

var (
	routeIDPattern         = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretNamePattern      = regexp.MustCompile(`^projects/[A-Za-z0-9-]+/secrets/[A-Za-z0-9_-]+$`)
	secretIDPattern        = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	versionPattern         = regexp.MustCompile(`^([1-9][0-9]*|[A-Za-z][A-Za-z0-9_-]*)$`)
)

// Adapter loads the initial single-document YAML workload format.
type Adapter struct{}

// New constructs a YAML workload adapter.
func New() *Adapter {
	return &Adapter{}
}

// LoadAll discovers and loads workload files in input order and directory lexical order.
func (adapter *Adapter) LoadAll(paths []string) ([]deploy.Workload, error) {
	var filenames []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect workload path %s: %w", path, err)
		}
		if !info.IsDir() {
			filenames = append(filenames, path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("read workload directory %s: %w", path, err)
		}
		directoryFiles := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			extension := strings.ToLower(filepath.Ext(entry.Name()))
			if extension == ".yaml" || extension == ".yml" {
				directoryFiles = append(directoryFiles, filepath.Join(path, entry.Name()))
			}
		}
		slices.Sort(directoryFiles)
		filenames = append(filenames, directoryFiles...)
	}
	if len(filenames) == 0 {
		return nil, errors.New("no workload files found")
	}

	workloads := make([]deploy.Workload, len(filenames))
	for index, filename := range filenames {
		workload, err := adapter.Load(filename)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", filename, err)
		}
		workloads[index] = workload
	}
	return workloads, nil
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

	var httpConfig *deploy.HTTPConfig
	if document.HTTP != nil {
		if !routeIDPattern.MatchString(document.Name) {
			return deploy.Workload{}, errors.New("HTTP workload name must be a lowercase DNS label")
		}
		if document.HTTP.ContainerPort < 1 || document.HTTP.ContainerPort > 65535 {
			return deploy.Workload{}, errors.New("http.containerPort must be between 1 and 65535")
		}
		if document.HTTP.StartupProbe == nil || document.HTTP.StartupProbe.HTTPGet == nil {
			return deploy.Workload{}, errors.New("http.startupProbe.httpGet.path is required")
		}
		path := document.HTTP.StartupProbe.HTTPGet.Path
		parsed, err := url.ParseRequestURI(path)
		if err != nil || path == "" || path[0] != '/' || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
			return deploy.Workload{}, errors.New("http.startupProbe.httpGet.path must be an origin-form absolute path")
		}
		httpConfig = &deploy.HTTPConfig{ContainerPort: uint16(document.HTTP.ContainerPort), StartupProbePath: path}
	}

	literals, references, err := document.Container.Environment.resolve(document.GCP.Project)
	if err != nil {
		return deploy.Workload{}, err
	}

	return deploy.Workload{
		Name: document.Name,
		GCP: deploy.GCP{
			Project:   document.GCP.Project,
			Instances: instances,
		},
		Container: deploy.ContainerConfig{
			Image:             document.Container.Image,
			Command:           document.Container.Command,
			Environment:       literals,
			SecretEnvironment: references,
		},
		HTTP: httpConfig,
	}, nil
}

type workloadDocument struct {
	Name      string            `yaml:"name"`
	GCP       gcpDocument       `yaml:"gcp"`
	Container containerDocument `yaml:"container"`
	HTTP      *httpDocument     `yaml:"http"`
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

type httpDocument struct {
	ContainerPort int                   `yaml:"containerPort"`
	StartupProbe  *startupProbeDocument `yaml:"startupProbe"`
}

type startupProbeDocument struct {
	HTTPGet *httpGetDocument `yaml:"httpGet"`
}

type httpGetDocument struct {
	Path string `yaml:"path"`
}

type environmentEntry struct {
	name       string
	value      string
	secretName string
	version    string
}

type strictEnvironment []environmentEntry

func (environment *strictEnvironment) UnmarshalYAML(node *yaml.Node) error {
	var entries strictEnvironment
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Tag != "!!str" || value.Tag != "!!str" {
				return errors.New("container.env keys and values must be strings")
			}
			entries = append(entries, environmentEntry{name: key.Value, value: value.Value})
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			fields, err := environmentFields(item, "name", "value", "valueFrom")
			if err != nil {
				return err
			}
			name, err := environmentString(fields["name"])
			if err != nil {
				return err
			}
			entry := environmentEntry{name: name}
			value, literal := fields["value"]
			from, secret := fields["valueFrom"]
			if literal == secret {
				return errors.New("container.env entries require exactly one of value or valueFrom")
			}
			if literal {
				entry.value, err = environmentString(value)
			} else {
				var source, reference map[string]*yaml.Node
				source, err = environmentFields(from, "secretKeyRef")
				if err == nil {
					reference, err = environmentFields(source["secretKeyRef"], "name", "key")
				}
				if err == nil {
					entry.secretName, err = environmentString(reference["name"])
				}
				if err == nil {
					entry.version, err = environmentString(reference["key"])
				}
				if err == nil && (entry.secretName == "" || entry.version == "") {
					err = errors.New("container.env secretKeyRef name and key are required")
				}
			}
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
	default:
		return errors.New("container.env must be a mapping or list")
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if !environmentNamePattern.MatchString(entry.name) || seen[entry.name] {
			return errors.New("container.env contains an invalid or duplicate environment name")
		}
		seen[entry.name] = true
	}
	*environment = entries
	return nil
}

func (environment strictEnvironment) resolve(project string) (map[string]string, map[string]string, error) {
	var literals, references map[string]string
	for _, entry := range environment {
		if entry.secretName == "" && entry.version == "" {
			if literals == nil {
				literals = make(map[string]string)
			}
			literals[entry.name] = entry.value
			continue
		}
		name := entry.secretName
		if secretIDPattern.MatchString(name) {
			name = "projects/" + project + "/secrets/" + name
		}
		if !secretNamePattern.MatchString(name) || !versionPattern.MatchString(entry.version) {
			return nil, nil, errors.New("container.env secretKeyRef requires a valid secret name and explicit version key")
		}
		if references == nil {
			references = make(map[string]string)
		}
		references[entry.name] = name + "/versions/" + entry.version
	}
	return literals, references, nil
}

// Custom YAML decoding must enforce known fields itself: Node.Decode does not
// inherit the outer decoder's KnownFields setting.
func environmentFields(node *yaml.Node, allowed ...string) (map[string]*yaml.Node, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, errors.New("invalid container.env entry")
	}
	fields := make(map[string]*yaml.Node)
	for index := 0; index < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Tag != "!!str" || !slices.Contains(allowed, key.Value) || fields[key.Value] != nil {
			return nil, errors.New("container.env contains an unknown or duplicate field")
		}
		fields[key.Value] = node.Content[index+1]
	}
	return fields, nil
}

func environmentString(node *yaml.Node) (string, error) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", errors.New("container.env fields must be strings")
	}
	return node.Value, nil
}
