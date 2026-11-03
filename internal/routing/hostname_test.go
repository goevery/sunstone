package routing

import (
	"strings"
	"testing"
)

func TestRequestHostnameCanonicalizesAuthorities(t *testing.T) {
	tests := []struct {
		authority string
		hostname  string
		ok        bool
	}{
		{authority: "storefront.example", hostname: "storefront.example", ok: true},
		{authority: "Storefront.Example", hostname: "storefront.example", ok: true},
		{authority: "STOREFRONT.EXAMPLE:8080", hostname: "storefront.example", ok: true},
		{authority: "storefront.example:0", hostname: "storefront.example", ok: true},
		{authority: "storefront.example.", hostname: "storefront.example", ok: true},
		{authority: "storefront", hostname: "storefront", ok: true},
		{authority: "127.0.0.1:8080", hostname: "127.0.0.1", ok: true},
		{authority: "[::1]:8080", hostname: "::1", ok: true},
		{authority: "[::1]", hostname: "::1", ok: true},
		{authority: "[::ffff:127.0.0.1]", hostname: "::ffff:127.0.0.1", ok: true},

		{authority: "[storefront.example]", ok: false},
		{authority: "[not-an-ip]", ok: false},
		{authority: "[127.0.0.1]", ok: false},
		{authority: "[storefront.example]x", ok: false},

		{authority: "", ok: false},
		{authority: ".", ok: false},
		{authority: strings.Repeat("a", 254), ok: false},
		{authority: "storefront.example:", ok: false},
		{authority: "storefront.example:notaport", ok: false},
		{authority: "https://storefront.example", ok: false},
		{authority: "storefront.example/path", ok: false},
		{authority: "storefront.example?query", ok: false},
		{authority: "storefront#fragment", ok: false},
		{authority: "storefront example", ok: false},
		{authority: " storefront.example", ok: false},
		{authority: "user@storefront.example", ok: false},
		{authority: "storefront.example]", ok: false},
		{authority: "storefront[example", ok: false},
		{authority: "storefront\texample", ok: false},
		{authority: "::1", ok: false},
		{authority: "[::1", ok: false},
		{authority: "[::1]x", ok: false},
		{authority: "[::1]:x", ok: false},
		{authority: "-storefront.example", ok: false},
		{authority: "storefront-.example", ok: false},
		{authority: "storefront..example", ok: false},
	}
	for _, test := range tests {
		hostname, ok := requestHostname(test.authority)
		if ok != test.ok || (test.ok && hostname != test.hostname) {
			t.Fatalf("requestHostname(%q) = %q, %v; want %q, %v", test.authority, hostname, ok, test.hostname, test.ok)
		}
	}
}

func TestValidateRouteRequiresStrictHostnames(t *testing.T) {
	base := Route{
		Name: "routes/storefront",
		Backend: Backend{
			Address:          "storefront-01234567:8080",
			ContainerID:      "container-id",
			StartupProbePath: "/readyz",
		},
	}
	if err := validateRoute(base); err == nil {
		t.Fatal("validateRoute accepted a route without hostnames")
	}
	base.Hostnames = []string{"storefront.example.com"}
	if err := validateRoute(base); err != nil {
		t.Fatalf("validateRoute rejected a valid hostname set: %v", err)
	}

	invalid := [][]string{
		nil,
		{},
		{""},
		{"Storefront.Example.com"},
		{"storefront.example.com."},
		{"storefront.example.com:80"},
		{"https://storefront.example.com"},
		{"storefront.example.com/app"},
		{"*.example.com"},
		{"storefront.example.com", "storefront.example.com"},
	}
	for _, hostnames := range invalid {
		route := base
		route.Hostnames = hostnames
		if err := validateRoute(route); err == nil || !strings.Contains(err.Error(), "invalid route request") {
			t.Fatalf("validateRoute accepted hostnames %v: %v", hostnames, err)
		}
	}
}

func TestApplyUpdateMaskReplacesHostnames(t *testing.T) {
	current := Route{
		Name:      "routes/storefront",
		Hostnames: []string{"old.example.com"},
		Backend:   Backend{Address: "storefront-01234567:8080", ContainerID: "container-1", StartupProbePath: "/readyz"},
	}
	requested := Route{
		Name:      "routes/storefront",
		Hostnames: []string{"new.example.com", "newer.example.com"},
		Backend:   Backend{Address: "storefront-89abcdef:8080", ContainerID: "container-2", StartupProbePath: "/healthz"},
	}

	merged := applyUpdateMask(&current, requested, []string{"hostnames", "backend.address"})
	if !routeEqual(merged, Route{
		Name:      "routes/storefront",
		Hostnames: []string{"new.example.com", "newer.example.com"},
		Backend:   Backend{Address: "storefront-89abcdef:8080", ContainerID: "container-1", StartupProbePath: "/readyz"},
	}) {
		t.Fatalf("masked update = %+v", merged)
	}

	replaced := applyUpdateMask(&current, requested, nil)
	if !routeEqual(replaced, requested) {
		t.Fatalf("full update = %+v", replaced)
	}
}

func TestValidateMaskAcceptsHostnamePath(t *testing.T) {
	if err := validateMask([]string{"hostnames", "backend.address"}); err != nil {
		t.Fatalf("validateMask rejected hostnames path: %v", err)
	}
	if err := validateMask([]string{"hostname"}); err == nil {
		t.Fatal("validateMask accepted singular hostname path")
	}
}

func TestRouteEqualComparesHostnamesAndBackend(t *testing.T) {
	base := Route{
		Name:      "routes/storefront",
		Hostnames: []string{"storefront.example.com"},
		Backend:   Backend{Address: "storefront-01234567:8080", ContainerID: "container-1", StartupProbePath: "/readyz"},
	}
	same := base
	same.Hostnames = []string{"storefront.example.com"}
	if !routeEqual(base, same) {
		t.Fatal("equal routes compared unequal")
	}

	differentHostnames := base
	differentHostnames.Hostnames = []string{"other.example.com"}
	if routeEqual(base, differentHostnames) {
		t.Fatal("routes with different hostnames compared equal")
	}

	differentBackend := base
	differentBackend.Backend.ContainerID = "container-2"
	if routeEqual(base, differentBackend) {
		t.Fatal("routes with different backends compared equal")
	}
}
