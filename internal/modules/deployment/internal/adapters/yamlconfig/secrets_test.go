package yamlconfig_test

import (
	"github.com/goevery/sunstone/internal/modules/deployment/internal/adapters/yamlconfig"
	"testing"
)

func TestRejectsAmbiguousAndMalformedEnvironment(t *testing.T) {
	cases := []string{
		`[{name: API_KEY, value: "x", valueFrom: {secretKeyRef: {name: key, key: latest}}}]`,
		`[{name: API_KEY}]`,
		`[{name: API_KEY, value: 1}]`,
		`[{name: API_KEY, value: "", extra: "x"}]`,
		`[{name: API_KEY, valueFrom: {extra: "x", secretKeyRef: {name: key, key: latest}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, key: latest, extra: x}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: "", key: ""}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, key: ""}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: projects/p/secrets/s/versions/1, key: latest}}}]`,
		`[{name: API_KEY, valueFrom: {secretKeyRef: {name: key, key: 1}}}]`,
		`[{name: API_KEY, value: x}, {name: API_KEY, value: y}]`,
		`{API_KEY: x, API_KEY: y}`,
		`{BAD-NAME: x}`,
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			filename := writeConfig(t, "name: jobs\ngcp:\n  project: acme-prod\n  instances: [{zone: z, name: vm}]\ncontainer:\n  image: example/jobs\n  env: "+input+"\n")
			if _, err := yamlconfig.New().Load(filename); err == nil {
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
        secretKeyRef: {name: api-key, key: latest}
    - name: DATABASE
      valueFrom:
        secretKeyRef: {name: projects/other-project/secrets/database, key: "3"}
`)
	workload, err := yamlconfig.New().Load(filename)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := workload.Container.Environment["EMPTY"]; !ok || value != "" {
		t.Fatal("empty literal lost")
	}
	if _, ok := workload.Container.Environment["API_KEY"]; ok {
		t.Fatal("secret entered literal environment")
	}
	if workload.Container.SecretEnvironment["API_KEY"] != "projects/acme-prod/secrets/api-key/versions/latest" || workload.Container.SecretEnvironment["DATABASE"] != "projects/other-project/secrets/database/versions/3" {
		t.Fatalf("references = %v", workload.Container.SecretEnvironment)
	}
}
