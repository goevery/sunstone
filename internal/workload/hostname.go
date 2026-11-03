package workload

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Hostname routing limits: DNS name bounds plus the claim limit one HTTP
// workload's hostname set may occupy.
const (
	maximumHostnameLength       = 253
	maximumHostnameLabelLength  = 63
	maximumHostnamesPerWorkload = 100
)

var hostnameLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidateHostname accepts one hostname exactly as it is routed: a
// lowercase DNS hostname without port, scheme, path, wildcard, or
// trailing dot. The single-label form is valid.
func ValidateHostname(hostname string) error {
	if hostname == "" {
		return errors.New("hostname must not be empty")
	}
	if len(hostname) > maximumHostnameLength {
		return fmt.Errorf("hostname %s exceeds %d characters", hostname, maximumHostnameLength)
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) > maximumHostnameLabelLength || !hostnameLabelPattern.MatchString(label) {
			return fmt.Errorf("hostname %s must be a lowercase DNS hostname without port, path, wildcard, or trailing dot", hostname)
		}
	}

	return nil
}

// ValidateHostnames accepts the complete hostname set one HTTP workload
// routes: at least one hostname, at most one hundred, each individually
// valid, and no duplicates.
func ValidateHostnames(hostnames []string) error {
	if len(hostnames) == 0 {
		return errors.New("at least one hostname is required")
	}
	if len(hostnames) > maximumHostnamesPerWorkload {
		return fmt.Errorf("at most %d hostnames are supported", maximumHostnamesPerWorkload)
	}
	seen := make(map[string]struct{}, len(hostnames))
	for _, hostname := range hostnames {
		if err := ValidateHostname(hostname); err != nil {
			return err
		}
		if _, duplicate := seen[hostname]; duplicate {
			return fmt.Errorf("duplicate hostname %s", hostname)
		}
		seen[hostname] = struct{}{}
	}

	return nil
}
