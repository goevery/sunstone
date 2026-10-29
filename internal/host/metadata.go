package host

import (
	"encoding/json"
	"strconv"
)

// Deployed metadata labels record the configuration a managed container was
// created from. They carry references, never payloads.
const (
	ManagedLabel           = "dev.sunstone.managed"
	WorkloadLabel          = "dev.sunstone.workload"
	KindLabel              = "dev.sunstone.kind"
	HTTPPortLabel          = "dev.sunstone.http.port"
	HTTPProbePathLabel     = "dev.sunstone.http.startup-probe-path"
	SecretEnvironmentLabel = "dev.sunstone.secret-env"
)

// Workload kinds recorded in the deployed metadata.
const (
	KindHTTP       = "http"
	KindBackground = "background"
)

// DeployedMetadata is the configuration recorded in a managed container's
// labels.
type DeployedMetadata struct {
	Kind              string
	HTTPPort          uint16
	StartupProbePath  string
	SecretEnvironment map[string]string
}

// DecodeMetadata parses deployed metadata labels. The second return is false
// when the labels are missing, malformed, or contradictory. An HTTP deploy
// can repair them by replacing the container and route; background deploys
// and name-only operations refuse the unclassifiable state.
func DecodeMetadata(labels map[string]string) (DeployedMetadata, bool) {
	if labels[ManagedLabel] != "true" {
		return DeployedMetadata{}, false
	}
	kind := labels[KindLabel]
	if kind != KindHTTP && kind != KindBackground {
		return DeployedMetadata{}, false
	}

	metadata := DeployedMetadata{Kind: kind}
	hasHTTPFields := labels[HTTPPortLabel] != "" || labels[HTTPProbePathLabel] != ""
	if kind == KindHTTP {
		port, err := strconv.ParseUint(labels[HTTPPortLabel], 10, 16)
		if err != nil || port == 0 || labels[HTTPProbePathLabel] == "" {
			return DeployedMetadata{}, false
		}
		metadata.HTTPPort = uint16(port)
		metadata.StartupProbePath = labels[HTTPProbePathLabel]
	} else if hasHTTPFields {
		return DeployedMetadata{}, false
	}

	if raw := labels[SecretEnvironmentLabel]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata.SecretEnvironment); err != nil || metadata.SecretEnvironment == nil {
			return DeployedMetadata{}, false
		}
	}

	return metadata, true
}
