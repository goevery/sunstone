package host

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/workload"
)

func TestRestartReplacementClonesDeployedHTTPConfiguration(t *testing.T) {
	f := newSecretHost(t)
	// The deployed container runs an explicit command resolved from its image;
	// inspection reports the actual effective values.
	f.inspectCmd.Store([]string{"bin/web"})
	candidate, err := f.host.Create(t.Context(), restartHTTPSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.forgetSecretState()
	tokens, accesses := f.tokens.Load(), f.accesses.Load()

	replacement, err := f.host.RestartReplacement(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}

	// Zero metadata or Secret Manager requests: restart never refetches, even
	// with an empty fetch cache.
	if f.tokens.Load() != tokens || f.accesses.Load() != accesses {
		t.Fatalf("tokens=%d accesses=%d", f.tokens.Load(), f.accesses.Load())
	}
	if replacement.ID == "" || !strings.HasPrefix(replacement.Name, "storefront-") {
		t.Fatalf("replacement = %+v", replacement)
	}
	namePattern := regexp.MustCompile(`^storefront-[a-f0-9]{8}$`)
	if !namePattern.MatchString(replacement.Name) {
		t.Fatalf("replacement name = %q", replacement.Name)
	}

	f.mu.Lock()
	created := f.createdRaw[len(f.createdRaw)-1]
	f.mu.Unlock()
	// The clone uses the immutable image ID, never the input tag.
	if created["Image"] != "image" {
		t.Fatalf("clone image = %v", created["Image"])
	}
	if command, ok := created["Cmd"].([]any); !ok || len(command) != 1 || command[0] != "bin/web" {
		t.Fatalf("clone command = %v", created["Cmd"])
	}
	environment, ok := created["Env"].([]any)
	if !ok || len(environment) != 2 {
		t.Fatalf("clone environment = %v", created["Env"])
	}
	entries := make(map[string]bool, len(environment))
	for _, entry := range environment {
		if text, ok := entry.(string); ok {
			entries[text] = true
		}
	}

	if !entries["API_KEY=vm-secret-value"] || !entries["LITERAL=keep"] {
		t.Fatalf("clone environment = %v", environment)
	}

	labels, ok := created["Labels"].(map[string]any)
	if !ok {
		t.Fatalf("clone labels = %v", created["Labels"])
	}
	if labels[KindLabel] != KindHTTP || labels[HTTPPortLabel] != "8080" ||
		labels[HTTPProbePathLabel] != "/readyz" {
		t.Fatalf("clone labels = %v", labels)
	}
	var references map[string]string
	if raw, ok := labels[SecretEnvironmentLabel].(string); !ok ||
		json.Unmarshal([]byte(raw), &references) != nil ||
		references["API_KEY"] != "projects/p/secrets/key/versions/latest" {
		t.Fatalf("clone secret references = %v", labels[SecretEnvironmentLabel])
	}

	hostConfig, ok := created["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("clone host config = %v", created["HostConfig"])
	}
	restartPolicy, ok := hostConfig["RestartPolicy"].(map[string]any)
	if !ok || restartPolicy["Name"] != "unless-stopped" {
		t.Fatalf("clone restart policy = %v", hostConfig["RestartPolicy"])
	}

	networking, ok := created["NetworkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("clone networking = %v", created["NetworkingConfig"])
	}
	endpoints, ok := networking["EndpointsConfig"].(map[string]any)
	if !ok || endpoints[workloadNetwork] == nil {
		t.Fatalf("clone networking endpoints = %v", networking["EndpointsConfig"])
	}
	exposed, ok := created["ExposedPorts"].(map[string]any)
	if !ok || exposed["8080/tcp"] == nil {
		t.Fatalf("clone exposed ports = %v", created["ExposedPorts"])
	}
}

func TestRestartReplacementUsesResolvedImageIDNotConfiguredTag(t *testing.T) {
	f := newSecretHost(t)
	candidate, err := f.host.Create(t.Context(), restartHTTPSpec())
	if err != nil {
		t.Fatal(err)
	}
	// Docker's Config.Image keeps the configured reference, which can be a
	// mutable tag; the top-level inspection Image is the resolved image ID.
	f.inspectImage.Store("sha256:0123456789abcdef")
	f.inspectConfigImage.Store("registry/workload:latest")

	if _, err := f.host.RestartReplacement(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	created := f.createdRaw[len(f.createdRaw)-1]
	f.mu.Unlock()
	if created["Image"] != "sha256:0123456789abcdef" {
		t.Fatalf("clone image = %v, want the resolved ID", created["Image"])
	}
	if text, _ := created["Image"].(string); strings.Contains(text, "registry/workload") {
		t.Fatalf("clone image used the configured tag: %v", created["Image"])
	}
}

func TestRestartReplacementPreservesDeployedRestartPolicy(t *testing.T) {
	f := newSecretHost(t)
	candidate, err := f.host.Create(t.Context(), restartHTTPSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.inspectRestart.Store("on-failure")
	f.inspectRetries.Store(3)

	if _, err := f.host.RestartReplacement(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	created := f.createdRaw[len(f.createdRaw)-1]
	f.mu.Unlock()
	hostConfig, ok := created["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("clone host config = %v", created["HostConfig"])
	}
	restartPolicy, ok := hostConfig["RestartPolicy"].(map[string]any)
	if !ok || restartPolicy["Name"] != "on-failure" {
		t.Fatalf("clone restart policy = %v", hostConfig["RestartPolicy"])
	}
	if retries, ok := restartPolicy["MaximumRetryCount"].(float64); !ok || retries != 3 {
		t.Fatalf("clone retry count = %v (%T)", restartPolicy["MaximumRetryCount"], restartPolicy["MaximumRetryCount"])
	}
}

func TestRestartReplacementMissingLocalImageFailsExplicitly(t *testing.T) {
	f := newSecretHost(t)
	candidate, err := f.host.Create(t.Context(), restartHTTPSpec())
	if err != nil {
		t.Fatal(err)
	}
	f.forgetSecretState()
	tokens, accesses := f.tokens.Load(), f.accesses.Load()

	f.inspectImage.Store("sha256:missing")
	f.createNotFound.Store(true)
	_, err = f.host.RestartReplacement(t.Context(), candidate)
	if err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("error = %v", err)
	}
	if f.tokens.Load() != tokens || f.accesses.Load() != accesses {
		t.Fatalf("tokens=%d accesses=%d", f.tokens.Load(), f.accesses.Load())
	}
}

func TestRestartReplacementRedactsDeployedSecretsWithoutFetching(t *testing.T) {
	f := newSecretHost(t)
	candidate, err := f.host.Create(t.Context(), restartHTTPSpec())
	if err != nil {
		t.Fatal(err)
	}
	// A fresh runtime must rely on restart-time enrollment, not the fetch
	// cache from the original deployment.
	f.forgetSecretState()
	tokens, accesses := f.tokens.Load(), f.accesses.Load()

	// The hostile Docker diagnostic embeds the deployed secret value; the
	// clone enrolls deployed values for redaction without fetching them.
	f.dockerFailure.Store(true)
	_, err = f.host.RestartReplacement(t.Context(), candidate)
	if err == nil || strings.Contains(err.Error(), "vm-secret-value") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %v", err)
	}
	if f.tokens.Load() != tokens || f.accesses.Load() != accesses {
		t.Fatalf("tokens=%d accesses=%d", f.tokens.Load(), f.accesses.Load())
	}
}

func restartHTTPSpec() ContainerSpec {
	return ContainerSpec{
		Name:              "storefront",
		Workload:          "storefront",
		Image:             Image{ID: "image"},
		Environment:       map[string]string{"LITERAL": "keep"},
		SecretEnvironment: map[string]string{"API_KEY": "projects/p/secrets/key/versions/latest"},
		RestartPolicy:     "unless-stopped",
		HTTP:              &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"},
	}
}
