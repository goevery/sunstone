package routing

import "errors"

var (
	// ErrNotFound indicates that a requested route does not exist.
	ErrNotFound = errors.New("route not found")
	// ErrFailedPrecondition indicates that another catch-all route already exists.
	ErrFailedPrecondition = errors.New("only one catch-all route is supported")
	// ErrInvalidArgument indicates invalid route-control input.
	ErrInvalidArgument = errors.New("invalid route request")
)

// Route describes the active catch-all route.
type Route struct {
	Name    string  `json:"name"`
	Backend Backend `json:"backend"`
}

// Backend identifies one loopback container endpoint.
type Backend struct {
	Address          string `json:"address"`
	ContainerID      string `json:"containerId"`
	StartupProbePath string `json:"startupProbePath"`
}
