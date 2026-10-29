package routing

import (
	"strings"
	"testing"
)

func TestAcceptsManagedContainerBackendAddress(t *testing.T) {
	route := Route{
		Name: "routes/storefront",
		Backend: Backend{
			Address:          "storefront-01234567:8080",
			ContainerID:      "container-id",
			StartupProbePath: "/readyz",
		},
	}
	if err := validateRoute(route); err != nil {
		t.Fatalf("validate managed container backend: %v", err)
	}
}

func TestRejectsUnmanagedNetworkBackendAddress(t *testing.T) {
	route := Route{
		Name: "routes/storefront",
		Backend: Backend{
			Address:          "metadata.google.internal:80",
			ContainerID:      "container-id",
			StartupProbePath: "/readyz",
		},
	}
	if err := validateRoute(route); err == nil {
		t.Fatal("accepted unmanaged network backend")
	}
}

func TestBackendAddressValidationBoundaries(t *testing.T) {
	longest := "a" + strings.Repeat("b", 45) + "-01234567"
	if len(longest) != 55 {
		t.Fatalf("fixture length = %d", len(longest))
	}
	maximum := "a" + strings.Repeat("b", 53) + "-01234567"
	if len(maximum) != 63 {
		t.Fatalf("maximum length = %d", len(maximum))
	}
	overlong := "a" + strings.Repeat("b", 54) + "-01234567"
	tests := []struct {
		name    string
		address string
		accept  bool
	}{
		{name: "loopback", address: "127.0.0.1:8080", accept: true},
		{name: "new managed name", address: "storefront-01234567:8080", accept: true},
		{name: "maximum length name", address: maximum + ":8080", accept: true},
		{name: "overlong name", address: overlong + ":8080", accept: false},
		{name: "uppercase name", address: "Storefront-01234567:8080", accept: false},
		{name: "short random suffix", address: "storefront-0123456:8080", accept: false},
		{name: "non-hex suffix", address: "storefront-0123456z:8080", accept: false},
		{name: "no suffix", address: "storefront:8080", accept: false},
		{name: "external host", address: "metadata.google.internal:80", accept: false},
		{name: "external URL", address: "https://example.com/readyz", accept: false},
		{name: "zero port", address: "storefront-01234567:0", accept: false},
		{name: "missing port", address: "storefront-01234567", accept: false},
	}
	for _, test := range tests {
		route := Route{
			Name: "routes/storefront",
			Backend: Backend{
				Address:          test.address,
				ContainerID:      "container-id",
				StartupProbePath: "/readyz",
			},
		}
		err := validateRoute(route)
		if test.accept && err != nil {
			t.Fatalf("%s: rejected valid backend address %q: %v", test.name, test.address, err)
		}
		if !test.accept && err == nil {
			t.Fatalf("%s: accepted invalid backend address %q", test.name, test.address)
		}
	}
}
