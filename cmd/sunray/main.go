//go:build linux

// Sunray is an opt-in Docker OCI runtime for VM-identity secret injection.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/goevery/sunstone/internal/modules/sunray"
)

func main() {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	runtime, err := sunray.New(sunray.Config{
		RuntimePath: "/usr/bin/runc", StateDirectory: "/run/sunray",
		MetadataEndpoint: "http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token",
		SecretEndpoint:   "https://secretmanager.googleapis.com", Client: &http.Client{Transport: transport, Timeout: 30 * time.Second},
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	})
	code := 1
	if err == nil {
		code, err = runtime.Run(context.Background(), os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sunray:", err)
		code = 1
	}
	os.Exit(code)
}
