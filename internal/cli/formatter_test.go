package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/host"
)

func TestNewFormatterRejectsMissingWriter(t *testing.T) {
	if _, err := NewFormatter(nil); err == nil {
		t.Fatal("accepted a missing writer")
	}
}

func TestReportRendersOneLinePerProgressFact(t *testing.T) {
	var output bytes.Buffer
	formatter, err := NewFormatter(&output)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []host.Progress{
		{Stage: host.StageConnecting, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StagePulling, Image: "example/storefront:v1", Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageStarting, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageRouting, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageDrained, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageHealthy, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageCurrent, Workload: "jobs", Instance: "jobs-1"},
		{Stage: host.StageRestarted, Workload: "jobs", Instance: "jobs-1"},
		{Stage: host.StageRemoved, Workload: "jobs", Instance: "jobs-1"},
	} {
		formatter.Report(fact)
	}

	if err := formatter.Err(); err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"storefront / web-1: connecting",
		"storefront / web-1: pulling example/storefront:v1",
		"storefront / web-1: starting",
		"storefront / web-1: routing",
		"storefront / web-1: drained",
		"storefront / web-1: healthy",
		"jobs / jobs-1: current",
		"jobs / jobs-1: restarted",
		"jobs / jobs-1: removed",
	}, "\n") + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestWriteStatusRendersObservationsExactly(t *testing.T) {
	var output bytes.Buffer
	formatter, err := NewFormatter(&output)
	if err != nil {
		t.Fatal(err)
	}
	formatter.WriteStatus([]deployment.WorkloadStatus{{
		Workload: "storefront",
		Targets: []deployment.TargetStatus{{
			Target: deployment.Target{Zone: "us-central1-a", Instance: "web-1"},
			Observation: host.Observation{
				State: host.StateCurrent,
				// Deployed metadata carries secret references, but no
				// metadata or payload is ever rendered.
				Containers: []host.Container{{
					ID:      "container-1",
					Name:    "web",
					Image:   "sha256:image",
					Running: true,
					Healthy: true,
					Metadata: &host.DeployedMetadata{Kind: host.KindHTTP, SecretEnvironment: map[string]string{
						"API_KEY": "projects/p/secrets/key/versions/1",
					}},
				}},
				Route: &host.Route{Address: "web:8080", ContainerID: "container-1"},
			},
		}, {
			Target:      deployment.Target{Zone: "us-central1-b", Instance: "web-2"},
			Observation: host.Observation{State: deployment.StateUnreachable},
			Error:       errors.New("connect: IAP unavailable"),
		}},
	}})

	if err := formatter.Err(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"storefront / web-1 (us-central1-a): current",
		"  container: container-1 name=web image=sha256:image running=true healthy=true",
		"  route: web:8080 -> container-1",
		"storefront / web-2 (us-central1-b): unreachable",
		"  error: connect: IAP unavailable",
	}, "\n") + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
	if strings.Contains(output.String(), "API_KEY") || strings.Contains(output.String(), "secrets/") {
		t.Fatalf("status rendered a secret or metadata payload: %q", output.String())
	}
}

func TestWriteResultsRendersEveryTargetOutcome(t *testing.T) {
	var output bytes.Buffer
	formatter, err := NewFormatter(&output)
	if err != nil {
		t.Fatal(err)
	}
	formatter.WriteResults([]deployment.WorkloadResult{{
		Workload: "storefront",
		Targets: []deployment.TargetResult{
			{Target: deployment.Target{Zone: "us-central1-a", Instance: "web-1"}, State: deployment.TargetCompleted, Changed: true, Container: "container-1"},
			{Target: deployment.Target{Zone: "us-central1-b", Instance: "web-2"}, State: deployment.TargetCompleted, Changed: false},
			{Target: deployment.Target{Zone: "us-central1-c", Instance: "web-3"}, State: deployment.TargetFailed, Error: errors.New("update route: not ready")},
			{Target: deployment.Target{Zone: "us-central1-d", Instance: "web-4"}, State: deployment.TargetNotAttempted},
		},
	}, {
		Workload: "jobs",
		Targets:  []deployment.TargetResult{{Target: deployment.Target{Zone: "us-central1-a", Instance: "jobs-1"}, State: deployment.TargetNotAttempted}},
	}})

	if err := formatter.Err(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"storefront / web-1 (us-central1-a): completed (changed)",
		"  container: container-1",
		"storefront / web-2 (us-central1-b): completed (unchanged)",
		"storefront / web-3 (us-central1-c): failed",
		"  error: update route: not ready",
		"storefront / web-4 (us-central1-d): not attempted",
		"jobs / jobs-1 (us-central1-a): not attempted",
	}, "\n") + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestWriteResultsPreflightFailureShowsNoTargetCompleted(t *testing.T) {
	var output bytes.Buffer
	formatter, err := NewFormatter(&output)
	if err != nil {
		t.Fatal(err)
	}
	formatter.WriteResults([]deployment.WorkloadResult{{
		Workload: "storefront",
		Targets: []deployment.TargetResult{
			{Target: deployment.Target{Zone: "us-central1-a", Instance: "web-1"}, State: deployment.TargetNotAttempted},
			{Target: deployment.Target{Zone: "us-central1-b", Instance: "web-2"}, State: deployment.TargetFailed, Error: errors.New("connect: IAP unavailable")},
			{Target: deployment.Target{Zone: "us-central1-c", Instance: "web-3"}, State: deployment.TargetNotAttempted},
		},
	}})

	if err := formatter.Err(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"storefront / web-1 (us-central1-a): not attempted",
		"storefront / web-2 (us-central1-b): failed",
		"  error: connect: IAP unavailable",
		"storefront / web-3 (us-central1-c): not attempted",
	}, "\n") + "\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
	if strings.Contains(output.String(), "completed") {
		t.Fatalf("preflight failure summary shows a completed target: %q", output.String())
	}
}

func TestWriteResultsWarnsOnlyForUnconfirmedFailures(t *testing.T) {
	var output bytes.Buffer
	formatter, err := NewFormatter(&output)
	if err != nil {
		t.Fatal(err)
	}
	formatter.WriteResults([]deployment.WorkloadResult{{
		Workload: "storefront",
		Targets: []deployment.TargetResult{
			{Target: deployment.Target{Zone: "us-central1-a", Instance: "web-1"}, State: deployment.TargetFailed, Error: errors.New("unhealthy")},
			{Target: deployment.Target{Zone: "us-central1-b", Instance: "web-2"}, State: deployment.TargetFailed, Error: errors.New("EOF"), EffectsUnconfirmed: true},
			{Target: deployment.Target{Zone: "us-central1-c", Instance: "web-3"}, State: deployment.TargetNotAttempted},
		},
	}})

	if err := formatter.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"storefront / web-1 (us-central1-a): failed",
		"  error: unhealthy",
		"storefront / web-2 (us-central1-b): failed",
		"  error: EOF",
		"  warning: remote changes may have occurred; inspect before retrying",
		"storefront / web-3 (us-central1-c): not attempted",
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("output = %q", output.String())
	}
	for index, line := range lines {
		if line != want[index] {
			t.Fatalf("line %d = %q, want %q", index, line, want[index])
		}
	}
	// The warning names no execution status and promises no retry; it is
	// attached only to the marked failure.
	if strings.Count(output.String(), "warning:") != 1 {
		t.Fatalf("warning rendered for unmarked targets: %q", output.String())
	}
}

func TestWriteFailureRetainsFirstErrorWithoutInterruptingProgress(t *testing.T) {
	failure := errors.New("disk full")
	writer := &failingWriter{err: failure}
	formatter, err := NewFormatter(writer)
	if err != nil {
		t.Fatal(err)
	}

	formatter.Report(host.Progress{Stage: host.StageConnecting, Workload: "jobs", Instance: "jobs-1"})
	formatter.Report(host.Progress{Stage: host.StagePulling, Image: "image", Workload: "jobs", Instance: "jobs-1"})
	formatter.Report(host.Progress{Stage: host.StageHealthy, Workload: "jobs", Instance: "jobs-1"})

	if !errors.Is(formatter.Err(), failure) {
		t.Fatalf("retained error = %v", formatter.Err())
	}
	if writer.calls != 1 {
		t.Fatalf("writes continued after failure: %d", writer.calls)
	}
}

func TestWriteFailureRetainsFirstErrorWithoutInterruptingResults(t *testing.T) {
	failure := errors.New("disk full")
	writer := &failingWriter{err: failure}
	formatter, err := NewFormatter(writer)
	if err != nil {
		t.Fatal(err)
	}

	formatter.WriteResults([]deployment.WorkloadResult{{
		Workload: "storefront",
		Targets:  []deployment.TargetResult{{Target: deployment.Target{Zone: "z", Instance: "web-1"}, State: deployment.TargetCompleted, Changed: true}},
	}})
	formatter.WriteStatus(nil)

	if !errors.Is(formatter.Err(), failure) {
		t.Fatalf("retained error = %v", formatter.Err())
	}
	if writer.calls != 1 {
		t.Fatalf("writes continued after failure: %d", writer.calls)
	}
}

type failingWriter struct {
	err   error
	calls int
}

func (w *failingWriter) Write([]byte) (int, error) {
	w.calls++

	return 0, w.err
}
