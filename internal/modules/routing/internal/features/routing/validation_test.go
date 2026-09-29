package routing

import "testing"

func TestAcceptsManagedContainerBackendAddress(t *testing.T) {
	route := Route{
		Name: "routes/storefront",
		Backend: Backend{
			Address:          "storefront-0123456789ab-01234567:8080",
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
