package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/goevery/sunstone/internal/workload"
	"github.com/moby/moby/client"
)

// RestartReplacement creates a uniquely named, stopped replacement for the
// deployed container current, cloned from its actual Docker inspection: the
// same resolved image ID, effective command and resolved environment, the
// deployed restart policy, and the deployed HTTP networking and metadata
// labels. It never pulls images or resolves secrets; a missing local image
// fails explicitly. Deployed secret values are enrolled for diagnostic
// redaction without any fetch.
func (h *DockerRuntime) RestartReplacement(ctx context.Context, current Container) (Container, error) {
	inspected, err := h.docker.ContainerInspect(ctx, current.ID, client.ContainerInspectOptions{})
	if err != nil {
		return Container{}, h.safeError(err)
	}
	config := inspected.Container.Config
	hostConfig := inspected.Container.HostConfig
	if config == nil || hostConfig == nil {
		return Container{}, errors.New("container has incomplete configuration")
	}
	if inspected.Container.Image == "" {
		return Container{}, errors.New("container has no resolved image")
	}
	metadata, ok := DecodeMetadata(config.Labels)
	if !ok || config.Labels[WorkloadLabel] == "" {
		return Container{}, errors.New("container has missing or invalid deployed metadata")
	}
	h.enrollDeployedSecrets(config.Env, metadata.SecretEnvironment)

	// Rebuild the deployed metadata labels from the decoded provenance; swap
	// in the resolved image ID, actual command, resolved environment, and the
	// full inspected restart policy.
	spec := ContainerSpec{
		Workload:          config.Labels[WorkloadLabel],
		RestartPolicy:     string(hostConfig.RestartPolicy.Name),
		SecretEnvironment: metadata.SecretEnvironment,
	}
	if metadata.Kind == KindHTTP {
		spec.HTTP = &workload.HTTPConfig{ContainerPort: metadata.HTTPPort, StartupProbePath: metadata.StartupProbePath}
	}

	suffix := make([]byte, containerNameRandomBytes)
	if _, err := rand.Read(suffix); err != nil {
		return Container{}, fmt.Errorf("generate container name: %w", err)
	}
	name := managedContainerName(spec.Workload, hex.EncodeToString(suffix))
	options := containerCreateOptions(spec, name)
	options.Config.Image = inspected.Container.Image
	options.Config.Cmd = slices.Clone(config.Cmd)
	options.Config.Env = slices.Clone(config.Env)
	options.HostConfig.RestartPolicy = hostConfig.RestartPolicy

	created, err := h.docker.ContainerCreate(ctx, options)
	if err != nil {
		return Container{}, h.mutationError("create replacement", err)
	}

	return Container{ID: created.ID, Name: name}, nil
}
