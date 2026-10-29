package workload_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/workload"
)

func TestLoadsFilesAndDirectoriesInDeterministicOrder(t *testing.T) {
	directory := t.TempDir()
	config := func(name string) []byte {
		return []byte("name: " + name + "\ngcp:\n  project: acme-prod\n  instances: [{zone: us-central1-a, name: vm-1}]\ncontainer:\n  image: example/workload\n")
	}
	first := filepath.Join(t.TempDir(), "first.yaml")
	if err := os.WriteFile(first, config("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "b.yml"), config("third"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "a.yaml"), config("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "ignored.txt"), config("ignored"), 0600); err != nil {
		t.Fatal(err)
	}

	workloads, err := workload.NewLoader().LoadAll([]string{first, directory})
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) != 3 || workloads[0].Name != "first" || workloads[1].Name != "second" || workloads[2].Name != "third" {
		t.Fatalf("workloads = %+v", workloads)
	}
}

func TestLoadsMinimalBackgroundWorkload(t *testing.T) {
	filename := writeConfig(t, `
name: storefront-jobs
gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: jobs-1
container:
  image: docker.io/example/jobs:v1
  command: ["bin/jobs"]
  env:
    APP_ENV: production
`)

	definition, err := workload.NewLoader().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Name != "storefront-jobs" || definition.GCP.Instances[0].Name != "jobs-1" {
		t.Fatalf("unexpected definition: %+v", definition)
	}
	if definition.Container.Environment["APP_ENV"] != "production" {
		t.Fatalf("unexpected environment: %v", definition.Container.Environment)
	}
}

func TestLoadsMultipleInstancesInSourceOrder(t *testing.T) {
	filename := writeConfig(t, `
name: storefront-jobs
gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: jobs-1
    - zone: us-central1-b
      name: jobs-1
container:
  image: docker.io/example/jobs:v1
`)

	definition, err := workload.NewLoader().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.GCP.Instances) != 2 ||
		definition.GCP.Instances[0].Zone != "us-central1-a" ||
		definition.GCP.Instances[1].Zone != "us-central1-b" {
		t.Fatalf("instances = %+v", definition.GCP.Instances)
	}
}

func TestRejectsInvalidInstanceLists(t *testing.T) {
	tests := map[string]string{
		"empty": `
name: storefront-jobs
gcp:
  project: acme-prod
  instances: []
container:
  image: example/jobs
`,
		"missing name": `
name: storefront-jobs
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a}]
container:
  image: example/jobs
`,
		"missing zone": `
name: storefront-jobs
gcp:
  project: acme-prod
  instances: [{name: jobs-1}]
container:
  image: example/jobs
`,
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := workload.NewLoader().Load(writeConfig(t, config))
			if err == nil {
				t.Fatal("expected invalid instance list to be rejected")
			}
		})
	}
}

func TestRejectsDuplicateInstances(t *testing.T) {
	filename := writeConfig(t, `
name: storefront-jobs
gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: jobs-1
    - zone: us-central1-a
      name: jobs-1
container:
  image: docker.io/example/jobs:v1
`)

	_, err := workload.NewLoader().Load(filename)
	if err == nil || !strings.Contains(err.Error(), "duplicate GCP instance us-central1-a/jobs-1") {
		t.Fatalf("expected duplicate instance error, got %v", err)
	}
}

func TestLoadsMinimalHTTPWorkload(t *testing.T) {
	filename := writeConfig(t, `
name: storefront
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a, name: web-1}]
container:
  image: example/storefront
http:
  containerPort: 8080
  startupProbe:
    httpGet:
      path: /readyz
`)

	definition, err := workload.NewLoader().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if definition.HTTP == nil || definition.HTTP.ContainerPort != 8080 || definition.HTTP.StartupProbePath != "/readyz" {
		t.Fatalf("HTTP configuration = %+v", definition.HTTP)
	}
}

func TestRejectsInvalidHTTPConfiguration(t *testing.T) {
	tests := map[string]string{
		"missing port": `startupProbe: {httpGet: {path: /readyz}}`,
		"invalid port": `containerPort: 70000
  startupProbe: {httpGet: {path: /readyz}}`,
		"missing probe": `containerPort: 8080`,
		"invalid path": `containerPort: 8080
  startupProbe: {httpGet: {path: readyz}}`,
	}
	for name, httpConfig := range tests {
		t.Run(name, func(t *testing.T) {
			filename := writeConfig(t, `
name: storefront
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a, name: web-1}]
container:
  image: example/storefront
http:
  `+httpConfig+"\n")
			if _, err := workload.NewLoader().Load(filename); err == nil {
				t.Fatal("expected invalid HTTP configuration")
			}
		})
	}
}

func TestLoadsHTTPWorkloadWithMultipleInstancesInSourceOrder(t *testing.T) {
	filename := writeConfig(t, `
name: storefront
gcp:
  project: acme-prod
  instances:
    - {zone: us-central1-a, name: web-1}
    - {zone: us-central1-b, name: web-2}
container:
  image: example/storefront
http:
  containerPort: 8080
  startupProbe:
    httpGet: {path: /readyz}
`)

	definition, err := workload.NewLoader().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.GCP.Instances) != 2 || definition.GCP.Instances[0].Name != "web-1" || definition.GCP.Instances[1].Name != "web-2" {
		t.Fatalf("instances = %+v", definition.GCP.Instances)
	}
}

func TestRejectsUnsupportedConfiguration(t *testing.T) {
	filename := writeConfig(t, `
name: storefront-web
gcp:
  project: acme-prod
  instances:
    - zone: us-central1-a
      name: web-1
container:
  image: docker.io/example/web:v1
http:
  port: 3000
`)

	_, err := workload.NewLoader().Load(filename)
	if err == nil || !strings.Contains(err.Error(), "field port not found") {
		t.Fatalf("expected unsupported field error, got %v", err)
	}
}

func TestRejectsNonStringEnvironmentValue(t *testing.T) {
	filename := writeConfig(t, `
name: storefront-jobs
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a, name: jobs-1}]
container:
  image: example/jobs
  env:
    DEBUG: true
`)

	_, err := workload.NewLoader().Load(filename)
	if err == nil {
		t.Fatal("expected non-string environment value to be rejected")
	}
}

func TestRejectsMultipleDocuments(t *testing.T) {
	filename := writeConfig(t, `
name: first
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a, name: jobs-1}]
container: {image: example/first}
---
name: second
`)

	_, err := workload.NewLoader().Load(filename)
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("expected multiple document error, got %v", err)
	}
}

func writeConfig(t *testing.T, config string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "workload.yaml")
	if err := os.WriteFile(filename, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	return filename
}

func TestRejectsAmbiguousAndMalformedEnvironment(t *testing.T) {
	cases := []string{
		`[{name: API_KEY, value: "x", valueFrom: {secretKeyRef: {name: key, version: latest}}}]`,
		`[{name: API_KEY}]`,
		`[{name: API_KEY, value: 1}]`,
		`[{name: API_KEY, value: "", extra: "x"}]`,
		`[{name: API_KEY, valueFrom: {extra: "x", secretKeyRef: {name: key, version: latest}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, version: latest, extra: x}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: "", version: ""}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, version: ""}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: projects/p/secrets/s/versions/1, version: latest}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, version: 1}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, key: latest}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, version: latest, key: latest}}}]`,
		`[{name: API_KEY, value: x}, {name: API_KEY, value: y}]`,
		`{API_KEY: x, API_KEY: y}`,
		`{BAD-NAME: x}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			filename := writeConfig(t, "name: jobs\ngcp:\n  project: acme-prod\n  instances: [{zone: z, name: vm}]\ncontainer:\n  image: example/jobs\n  env: "+input+"\n")
			if _, err := workload.NewLoader().Load(filename); err == nil {
				t.Fatal("invalid environment accepted")
			}
		})
	}
}

func TestLoadsLiteralAndSecretEnvironmentWithoutPayloads(t *testing.T) {
	filename := writeConfig(t, `name: jobs
gcp:
  project: acme-prod
  instances: [{zone: us-central1-a, name: vm}]
container:
  image: example/jobs
  env:
    - name: EMPTY
      value: ""
    - name: API_KEY
      valueFrom:
        secretKeyRef: {name: api-key, version: latest}
    - name: DATABASE
      valueFrom:
        secretKeyRef: {name: projects/other-project/secrets/database, version: "3"}
`)
	definition, err := workload.NewLoader().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := definition.Container.Environment["EMPTY"]; !ok || value != "" {
		t.Fatal("empty literal lost")
	}
	if _, ok := definition.Container.Environment["API_KEY"]; ok {
		t.Fatal("secret entered literal environment")
	}
	if definition.Container.SecretEnvironment["API_KEY"] != "projects/acme-prod/secrets/api-key/versions/latest" || definition.Container.SecretEnvironment["DATABASE"] != "projects/other-project/secrets/database/versions/3" {
		t.Fatalf("references = %v", definition.Container.SecretEnvironment)
	}
}
