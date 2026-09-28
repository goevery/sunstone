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
	if err == nil || !strings.Contains(err.Error(), "field http not found") {
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
