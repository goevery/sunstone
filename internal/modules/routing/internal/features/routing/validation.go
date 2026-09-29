package routing

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	routeIDPattern     = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	backendNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?-[a-f0-9]{12}-[a-f0-9]{8}$`)
)

func validateRoute(route Route) error {
	if err := validateRouteName(route.Name); err != nil {
		return err
	}
	if route.Backend.ContainerID == "" {
		return fmt.Errorf("%w: backend address, container_id, and startup_probe_path are required", ErrInvalidArgument)
	}
	host, port, err := net.SplitHostPort(route.Backend.Address)
	parsedPort, portErr := strconv.ParseUint(port, 10, 16)
	ip := net.ParseIP(host)
	if err != nil || portErr != nil || parsedPort == 0 || len(host) > 63 || !(ip != nil && ip.IsLoopback() || backendNamePattern.MatchString(host)) {
		return fmt.Errorf("%w: backend address must identify a managed container and port", ErrInvalidArgument)
	}
	probe, err := url.ParseRequestURI(route.Backend.StartupProbePath)
	if err != nil || !strings.HasPrefix(route.Backend.StartupProbePath, "/") || probe.IsAbs() || probe.Host != "" || probe.Fragment != "" {
		return fmt.Errorf("%w: startup probe path must be an origin-form absolute path", ErrInvalidArgument)
	}
	return nil
}

func validateRouteName(name string) error {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || parts[0] != "routes" || !routeIDPattern.MatchString(parts[1]) {
		return fmt.Errorf("%w: route name must have format routes/{route}", ErrInvalidArgument)
	}
	return nil
}

func applyUpdateMask(current *Route, requested Route, paths []string) Route {
	if current == nil || len(paths) == 0 || slices.Contains(paths, "*") || slices.Contains(paths, "backend") {
		return requested
	}
	result := *current
	for _, path := range paths {
		switch path {
		case "backend.address":
			result.Backend.Address = requested.Backend.Address
		case "backend.container_id":
			result.Backend.ContainerID = requested.Backend.ContainerID
		case "backend.startup_probe_path":
			result.Backend.StartupProbePath = requested.Backend.StartupProbePath
		}
	}
	return result
}

func validateMask(paths []string) error {
	for _, path := range paths {
		if !slices.Contains([]string{"*", "backend", "backend.address", "backend.container_id", "backend.startup_probe_path"}, path) {
			return fmt.Errorf("%w: unsupported update_mask path %q", ErrInvalidArgument, path)
		}
	}
	return nil
}
