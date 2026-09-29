package routing

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
)

var routeIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validateRoute(route *sunbeampb.Route) error {
	if err := validateRouteName(route); err != nil {
		return err
	}
	candidate := route.GetBackend()
	if candidate == nil || candidate.GetContainerId() == "" {
		return errors.New("backend address, container_id, and startup_probe_path are required")
	}
	host, _, err := net.SplitHostPort(candidate.GetAddress())
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("backend address must be a loopback IP and port")
	}
	probe, err := url.ParseRequestURI(candidate.GetStartupProbePath())
	if err != nil || !strings.HasPrefix(candidate.GetStartupProbePath(), "/") || probe.IsAbs() || probe.Host != "" || probe.Fragment != "" {
		return errors.New("startup probe path must be an origin-form absolute path")
	}
	return nil
}

func validateRouteName(route *sunbeampb.Route) error {
	if route == nil {
		return errors.New("route is required")
	}
	parts := strings.Split(route.GetName(), "/")
	if len(parts) != 2 || parts[0] != "routes" || !routeIDPattern.MatchString(parts[1]) {
		return errors.New("route name must have format routes/{route}")
	}
	return nil
}

func applyUpdateMask(current, requested *sunbeampb.Route, paths []string) *sunbeampb.Route {
	if current == nil || len(paths) == 0 || slices.Contains(paths, "*") || slices.Contains(paths, "backend") {
		return cloneRoute(requested)
	}
	result := cloneRoute(current)
	for _, path := range paths {
		switch path {
		case "backend.address":
			result.Backend.Address = requested.GetBackend().GetAddress()
		case "backend.container_id":
			result.Backend.ContainerId = requested.GetBackend().GetContainerId()
		case "backend.startup_probe_path":
			result.Backend.StartupProbePath = requested.GetBackend().GetStartupProbePath()
		}
	}
	return result
}

func validateMask(paths []string) error {
	for _, path := range paths {
		if !slices.Contains([]string{"*", "backend", "backend.address", "backend.container_id", "backend.startup_probe_path"}, path) {
			return fmt.Errorf("unsupported update_mask path %q", path)
		}
	}
	return nil
}
