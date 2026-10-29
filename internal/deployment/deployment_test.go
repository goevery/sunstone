package deployment

import (
	"testing"
	"time"

	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/workload"
)

func TestNewGroupRequiresProvider(t *testing.T) {
	if _, err := NewGroup(nil, silentReporter(), Config{PreflightTimeout: testPreflightTimeout}); err == nil {
		t.Fatal("accepted a missing host provider")
	}
}

func TestNewGroupRequiresReporter(t *testing.T) {
	if _, err := NewGroup(&stubProvider{}, nil, Config{PreflightTimeout: testPreflightTimeout}); err == nil {
		t.Fatal("accepted a missing progress reporter")
	}
}

func TestNewGroupRequiresPreflightTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewGroup(&stubProvider{}, silentReporter(), Config{PreflightTimeout: timeout}); err == nil {
			t.Fatalf("accepted preflight timeout %v", timeout)
		}
	}
}

func TestSelectionOfDerivesTargetsFromDefinition(t *testing.T) {
	selection := SelectionOf(workload.Definition{
		Name: "storefront",
		GCP: workload.GCP{
			Project:   "acme-prod",
			Instances: []workload.Instance{{Zone: "us-central1-a", Name: "web-1"}, {Zone: "us-central1-b", Name: "web-2"}},
		},
	})
	if selection.Workload != "storefront" || len(selection.Targets) != 2 {
		t.Fatalf("selection = %+v", selection)
	}
	if selection.Targets[0] != (Target{Project: "acme-prod", Zone: "us-central1-a", Instance: "web-1"}) {
		t.Fatalf("first target = %+v", selection.Targets[0])
	}
	if selection.Targets[1] != (Target{Project: "acme-prod", Zone: "us-central1-b", Instance: "web-2"}) {
		t.Fatalf("second target = %+v", selection.Targets[1])
	}
}

func silentReporter() host.Reporter {
	return func(host.Progress) {}
}
