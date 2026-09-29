package yamlconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/yamlconfig"
)

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

	workload, err := yamlconfig.New().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if workload.Name != "storefront-jobs" || workload.GCP.Instances[0].Name != "jobs-1" {
		t.Fatalf("unexpected workload: %+v", workload)
	}
	if workload.Container.Environment["APP_ENV"] != "production" {
		t.Fatalf("unexpected environment: %v", workload.Container.Environment)
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

	workload, err := yamlconfig.New().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if len(workload.GCP.Instances) != 2 ||
		workload.GCP.Instances[0].Zone != "us-central1-a" ||
		workload.GCP.Instances[1].Zone != "us-central1-b" {
		t.Fatalf("instances = %+v", workload.GCP.Instances)
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
			_, err := yamlconfig.New().Load(writeConfig(t, config))
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

	_, err := yamlconfig.New().Load(filename)
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

	workload, err := yamlconfig.New().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if workload.HTTP == nil || workload.HTTP.ContainerPort != 8080 || workload.HTTP.StartupProbePath != "/readyz" {
		t.Fatalf("HTTP configuration = %+v", workload.HTTP)
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
			if _, err := yamlconfig.New().Load(filename); err == nil {
				t.Fatal("expected invalid HTTP configuration")
			}
		})
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

	_, err := yamlconfig.New().Load(filename)
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

	_, err := yamlconfig.New().Load(filename)
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

	_, err := yamlconfig.New().Load(filename)
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
