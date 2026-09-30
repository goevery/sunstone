//go:build linux

// Package sunray prepares secret-backed OCI processes and delegates to runc.
package sunray

import (
	"context"
	"io"
	"net/http"

	"github.com/goevery/sunstone/internal/modules/sunray/internal/adapters/google"
	"github.com/goevery/sunstone/internal/modules/sunray/internal/features/runtime"
)

// Module implements the runtime executable's lifecycle operation.
type Module interface {
	Run(context.Context, []string) (int, error)
}

// Config supplies trusted host integrations; all fields are required.
// Endpoint and runtime settings are composition-root settings, not workload input.
type Config struct {
	RuntimePath      string
	StateDirectory   string
	MetadataEndpoint string
	SecretEndpoint   string
	Client           *http.Client
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
}

// New constructs a Linux OCI runtime. State and secret preparation require tmpfs.
func New(config Config) (Module, error) {
	source, err := google.New(config.Client, config.MetadataEndpoint, config.SecretEndpoint)
	if err != nil {
		return nil, err
	}
	return runtime.New(config.RuntimePath, config.StateDirectory, source, config.Stdin, config.Stdout, config.Stderr)
}
