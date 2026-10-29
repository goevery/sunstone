package deployment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goevery/sunstone/internal/host"
	"github.com/goevery/sunstone/internal/workload"
)

// testPreflightTimeout bounds checks in group tests unless a test probes
// timeout behavior with a tighter value.
const testPreflightTimeout = 30 * time.Second

func TestDeployChecksAllTargetsBeforeAnyMutation(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
	}
	var events []string
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				events = append(events, "deploy-web-1")

				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		"web-2": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				events = append(events, "deploy-web-2")

				return host.Result{Container: "container-2", Changed: true}, nil
			},
		},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err != nil {
		t.Fatal(err)
	}
	assertStates(t, result, []string{TargetCompleted, TargetCompleted})
	if result.Targets[0].Container != "container-1" || !result.Targets[0].Changed ||
		result.Targets[1].Container != "container-2" || !result.Targets[1].Changed {
		t.Fatalf("target results = %+v", result.Targets)
	}
	// Every target is opened and checked before the first mutation; each
	// handle closes right after its mutation.
	assertStrings(t, events, []string{
		"connect-web-1", "connect-web-2",
		"deploy-web-1", "close-web-1",
		"deploy-web-2", "close-web-2",
	})
}

func TestDeployPreflightFailurePreventsAllMutations(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
		{Zone: "us-central1-c", Name: "web-3"},
	}
	var events []string
	checkFailure := errors.New("check Sunbeam access: sunbeam control refused")
	// Deploy functions stay unset: any mutation fails the test by panicking.
	hosts := map[string]Host{
		"web-1": &scriptedHost{check: passingCheck},
		"web-2": &scriptedHost{check: func(context.Context, string, bool) error {
			return checkFailure
		}},
		"web-3": &scriptedHost{check: passingCheck},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "preflight storefront on web-2: check Sunbeam access") {
		t.Fatalf("error = %v", err)
	}
	// The failing target reports its check error; every other target stays
	// unmutated and not attempted.
	assertStates(t, result, []string{TargetNotAttempted, TargetFailed, TargetNotAttempted})
	if !errors.Is(result.Targets[1].Error, checkFailure) {
		t.Fatalf("failed target error = %v", result.Targets[1].Error)
	}
	// Nothing was mutated anywhere, so no failure claims unconfirmed
	// effects.
	for index, target := range result.Targets {
		if target.EffectsUnconfirmed {
			t.Fatalf("target %d claimed unconfirmed effects before any mutation", index)
		}
	}
	// Every opened handle is released exactly once: the failed check's
	// handle immediately, the prepared ones after preflight aborts.
	assertStrings(t, events, []string{
		"connect-web-1", "connect-web-2", "close-web-2", "connect-web-3",
		"close-web-1", "close-web-3",
	})
}

func TestDeployPreflightConnectFailurePreventsAllMutations(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
		{Zone: "us-central1-c", Name: "web-3"},
	}
	var events []string
	// Only web-1 has a host: web-2 and web-3 both fail to open during
	// preflight, so every failure is collected and nothing is mutated.
	hosts := map[string]Host{"web-1": &scriptedHost{check: passingCheck}}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "preflight storefront on web-2: connect: IAP unavailable") {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, result, []string{TargetNotAttempted, TargetFailed, TargetFailed})
	if result.Targets[1].Error == nil || !strings.Contains(result.Targets[1].Error.Error(), "IAP unavailable") {
		t.Fatalf("failed target error = %v", result.Targets[1].Error)
	}
	// web-1 passed its check but was never mutated; its prepared handle is
	// released exactly once.
	assertStrings(t, events, []string{"connect-web-1", "connect-web-2", "connect-web-3", "close-web-1"})
}

func TestDeployPrefailureReusesPreparedHandlesWithoutReopening(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
	}
	var events []string
	first := &recordingHost{}
	second := &recordingHost{}
	hosts := map[string]Host{"web-1": first.script(passingCheck), "web-2": second.script(passingCheck)}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := group.Deploy(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	for _, host := range []*recordingHost{first, second} {
		if host.checks != 1 || host.deploys != 1 {
			t.Fatalf("checks = %d, deploys = %d", host.checks, host.deploys)
		}
	}
	// Each target opens exactly once: the apply pass reuses the prepared
	// handle instead of rerunning connection setup.
	assertStrings(t, events, []string{"connect-web-1", "connect-web-2", "close-web-1", "close-web-2"})
}

func TestDeployPassesDesiredHTTPFromDefinition(t *testing.T) {
	var desired []bool
	var events []string
	hosts := map[string]Host{"web-1": &scriptedHost{
		check: func(_ context.Context, _ string, http bool) error {
			desired = append(desired, http)

			return nil
		},
		deploy:  func(context.Context, workload.Definition) (host.Result, error) { return host.Result{}, nil },
		restart: func(context.Context, string) (host.Result, error) { return host.Result{}, nil },
	}}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	httpDefinition := storefrontDefinition()
	httpDefinition.HTTP = &workload.HTTPConfig{ContainerPort: 8080, StartupProbePath: "/readyz"}
	if _, err := group.Deploy(t.Context(), httpDefinition); err != nil {
		t.Fatal(err)
	}
	if _, err := group.Restart(t.Context(), []Selection{{Workload: "storefront", Targets: []Target{{Instance: "web-1"}}}}); err != nil {
		t.Fatal(err)
	}
	// Deploy derives the requirement from the desired definition; the
	// name-only restart follows deployed metadata instead.
	assertBools(t, desired, []bool{true, false})
}

func TestRestartPreflightFailureAcrossWorkloadsPreventsAllMutations(t *testing.T) {
	selections := []Selection{
		{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}}},
		{Workload: "jobs", Targets: []Target{{Zone: "c", Instance: "jobs-1"}}},
	}
	var events []string
	checkFailure := errors.New("check Docker access: docker daemon unreachable")
	// Restart functions stay unset: any mutation fails the test by panicking.
	hosts := map[string]Host{
		"web-1":  &scriptedHost{check: passingCheck},
		"jobs-1": &scriptedHost{check: func(context.Context, string, bool) error { return checkFailure }},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	results, err := group.Restart(t.Context(), selections)
	if err == nil || !strings.Contains(err.Error(), "preflight jobs on jobs-1") {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, results[0], []string{TargetNotAttempted})
	assertStates(t, results[1], []string{TargetFailed})
	if !errors.Is(results[1].Targets[0].Error, checkFailure) {
		t.Fatalf("failed target error = %v", results[1].Targets[0].Error)
	}
	assertStrings(t, events, []string{"connect-web-1", "connect-jobs-1", "close-jobs-1", "close-web-1"})
}

func TestPreflightTimeoutFailsTargetWithoutMutations(t *testing.T) {
	definition := storefrontDefinition()
	var events []string
	hosts := map[string]Host{
		"web-1": &scriptedHost{check: func(ctx context.Context, _ string, _ bool) error {
			time.Sleep(30 * time.Millisecond)

			return ctx.Err()
		}},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "preflight storefront on web-1") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, result, []string{TargetFailed})
	assertStrings(t, events, []string{"connect-web-1", "close-web-1"})
}

func TestPreflightCancellationStopsRemainingChecks(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var events []string
	hosts := map[string]Host{
		"web-1": &scriptedHost{check: func(context.Context, string, bool) error {
			cancel()

			return nil
		}},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(ctx, definition)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "preflight storefront on web-2") {
		t.Fatalf("error = %v", err)
	}
	// Cancellation stops the remaining checks; nothing is mutated, and the
	// handle prepared before cancellation is released exactly once.
	assertStates(t, result, []string{TargetNotAttempted, TargetNotAttempted})
	assertStrings(t, events, []string{"connect-web-1", "close-web-1"})
}

func TestDeployReportsUnconfirmedEffectsOnLostMutationAnswer(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
		{Zone: "us-central1-c", Name: "web-3"},
	}
	var events []string
	// web-3 passes preflight; its deploy function stays unset: it is
	// never reached and must not be mutated.
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		"web-2": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{}, &host.UnconfirmedError{Operation: "start container", Cause: errors.New("EOF")}
			},
		},
		"web-3": &scriptedHost{check: passingCheck},
	}
	group, err := NewGroup(hostProvider(&events, hosts, map[string]error{"web-2": errors.New("close failed")}), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !host.EffectsUnconfirmed(err) {
		t.Fatalf("error = %v", err)
	}
	// The uncertainty is a separate flag on the failed target, not another
	// execution state; completed and not-attempted targets stay unmarked.
	assertStates(t, result, []string{TargetCompleted, TargetFailed, TargetNotAttempted})
	if !result.Targets[1].EffectsUnconfirmed {
		t.Fatal("failed target did not report unconfirmed effects")
	}
	if result.Targets[0].EffectsUnconfirmed || result.Targets[2].EffectsUnconfirmed {
		t.Fatalf("uncertainty leaked to other targets: %+v", result.Targets)
	}
	// The joined close error preserves the marker's identity.
	if !host.EffectsUnconfirmed(result.Targets[1].Error) || !strings.Contains(result.Targets[1].Error.Error(), "close failed") {
		t.Fatalf("failed target error = %v", result.Targets[1].Error)
	}
}

func TestDeployStopsAfterHostFailure(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
		{Zone: "us-central1-c", Name: "web-3"},
	}
	var events []string
	failure := errors.New("update route replacement: not ready")
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		"web-2": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{}, failure
			},
		},
		// web-3 passes preflight but its deploy function stays unset: it is
		// never reached and must not be mutated.
		"web-3": &scriptedHost{check: passingCheck},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "deploy storefront on web-2: update route replacement: not ready") {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, result, []string{TargetCompleted, TargetFailed, TargetNotAttempted})
	if result.Targets[0].Container != "container-1" || !result.Targets[0].Changed {
		t.Fatalf("prior success erased: %+v", result.Targets[0])
	}
	if !errors.Is(result.Targets[1].Error, failure) {
		t.Fatalf("failed target error = %v", result.Targets[1].Error)
	}
	assertStrings(t, events, []string{
		"connect-web-1", "connect-web-2", "connect-web-3",
		"close-web-1", "close-web-2", "close-web-3",
	})
}

func TestDeployCloseFailureRetainsSuccessfulOutcomeAndStops(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
	}
	var events []string
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		// web-2 passes preflight; its deploy function stays unset.
		"web-2": &scriptedHost{check: passingCheck},
	}
	group, err := NewGroup(hostProvider(&events, hosts, map[string]error{"web-1": errors.New("close failed")}), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "deploy storefront on web-1: close target: close failed") {
		t.Fatalf("error = %v", err)
	}
	// The mutation is known-successful; its outcome survives the cleanup
	// failure, and the rollout stops before the next target.
	assertStates(t, result, []string{TargetCompleted, TargetNotAttempted})
	if result.Targets[0].Container != "container-1" || !result.Targets[0].Changed {
		t.Fatalf("successful outcome erased: %+v", result.Targets[0])
	}
	assertStrings(t, events, []string{"connect-web-1", "connect-web-2", "close-web-1", "close-web-2"})
}

func TestDeployFailureRetainsCloseError(t *testing.T) {
	definition := storefrontDefinition()
	failure := errors.New("verify replacement: unhealthy")
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				return host.Result{}, failure
			},
		},
	}
	var events []string
	group, err := NewGroup(hostProvider(&events, hosts, map[string]error{"web-1": errors.New("close failed")}), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(t.Context(), definition)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, result, []string{TargetFailed})
	if !errors.Is(result.Targets[0].Error, failure) || !strings.Contains(result.Targets[0].Error.Error(), "close failed") {
		t.Fatalf("failed target error = %v", result.Targets[0].Error)
	}
}

func TestDeployCancellationStopsBeforeNextTarget(t *testing.T) {
	definition := storefrontDefinition()
	definition.GCP.Instances = []workload.Instance{
		{Zone: "us-central1-a", Name: "web-1"},
		{Zone: "us-central1-b", Name: "web-2"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				cancel()

				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		// web-2 passes preflight; its deploy function stays unset.
		"web-2": &scriptedHost{check: passingCheck},
	}
	var events []string
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	result, err := group.Deploy(ctx, definition)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "deploy storefront on web-2") {
		t.Fatalf("error = %v", err)
	}
	assertStates(t, result, []string{TargetCompleted, TargetNotAttempted})
	assertStrings(t, events, []string{"connect-web-1", "connect-web-2", "close-web-1", "close-web-2"})
}

func TestDeployEnrichesHostProgressWithTargetIdentity(t *testing.T) {
	var facts []host.Progress
	var openedReporter host.Reporter
	provider := &stubProvider{open: func(_ context.Context, _ Target, report host.Reporter) (Handle, error) {
		openedReporter = report

		return Handle{
			Host: &scriptedHost{
				check: passingCheck,
				deploy: func(context.Context, workload.Definition) (host.Result, error) {
					openedReporter(host.Progress{Stage: host.StageHealthy})

					return host.Result{Container: "container-1"}, nil
				},
			},
			Close: func() error { return nil },
		}, nil
	}}
	group, err := NewGroup(provider, func(fact host.Progress) { facts = append(facts, fact) }, Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := group.Deploy(t.Context(), storefrontDefinition()); err != nil {
		t.Fatal(err)
	}
	// Connecting is reported once, during preflight preparation, and host
	// facts arrive enriched with target identity.
	want := []host.Progress{
		{Stage: host.StageConnecting, Workload: "storefront", Instance: "web-1"},
		{Stage: host.StageHealthy, Workload: "storefront", Instance: "web-1"},
	}
	if len(facts) != len(want) {
		t.Fatalf("facts = %+v", facts)
	}
	for index, fact := range facts {
		if fact != want[index] {
			t.Fatalf("facts = %+v, want %+v", facts, want)
		}
	}
}

func TestDeployBoundsOpenAndCheckWithOnePerTargetContext(t *testing.T) {
	// Opening and checking are bounded by one per-target preflight context;
	// it is canceled once the check passes, and the prepared handle still
	// serves the apply pass — established sessions own their transport
	// lifetime and survive the setup context's cancellation.
	var openedWith, checkedWith context.Context
	var applied bool
	provider := &stubProvider{open: func(ctx context.Context, _ Target, _ host.Reporter) (Handle, error) {
		openedWith = ctx

		return Handle{Host: &scriptedHost{
			check: func(checkCtx context.Context, workloadName string, desiredHTTP bool) error {
				checkedWith = checkCtx

				return passingCheck(checkCtx, workloadName, desiredHTTP)
			},
			deploy: func(context.Context, workload.Definition) (host.Result, error) {
				applied = true

				return host.Result{Container: "container-1", Changed: true}, nil
			},
		}, Close: func() error { return nil }}, nil
	}}
	group, err := NewGroup(provider, silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := group.Deploy(t.Context(), storefrontDefinition()); err != nil {
		t.Fatal(err)
	}
	if openedWith == nil || checkedWith == nil || checkedWith != openedWith {
		t.Fatal("open and check did not share one bounded context")
	}
	// The bounded context is canceled once the check passes, while the
	// operation context and the prepared handle keep serving apply.
	if openedWith.Err() == nil {
		t.Fatal("preflight context was not canceled after the check")
	}
	if err := t.Context().Err(); err != nil {
		t.Fatalf("operation context canceled: %v", err)
	}
	if !applied {
		t.Fatal("apply did not run on the prepared handle")
	}
}

func TestRestartRunsSelectionsInOrderAndStopsOnFailure(t *testing.T) {
	selections := []Selection{
		{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}, {Zone: "b", Instance: "web-2"}}},
		{Workload: "jobs", Targets: []Target{{Zone: "c", Instance: "jobs-1"}}},
	}
	var events []string
	var restarted []string
	failure := errors.New("workload is missing")
	// jobs-1 passes preflight; its restart function stays unset: the
	// rollout stops before reaching it.
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			restart: func(_ context.Context, workloadName string) (host.Result, error) {
				restarted = append(restarted, workloadName)

				return host.Result{Container: "container-1", Changed: true}, nil
			},
		},
		"web-2": &scriptedHost{
			check: passingCheck,
			restart: func(_ context.Context, workloadName string) (host.Result, error) {
				restarted = append(restarted, workloadName)

				return host.Result{}, failure
			},
		},
		"jobs-1": &scriptedHost{check: passingCheck},
	}
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	results, err := group.Restart(t.Context(), selections)
	if err == nil || !strings.Contains(err.Error(), "restart storefront on web-2: workload is missing") {
		t.Fatalf("error = %v", err)
	}
	if len(results) != 2 || results[0].Workload != "storefront" || results[1].Workload != "jobs" {
		t.Fatalf("results = %+v", results)
	}
	assertStates(t, results[0], []string{TargetCompleted, TargetFailed})
	assertStates(t, results[1], []string{TargetNotAttempted})
	assertStrings(t, restarted, []string{"storefront", "storefront"})
	assertStrings(t, events, []string{
		"connect-web-1", "connect-web-2", "connect-jobs-1",
		"close-web-1", "close-web-2", "close-jobs-1",
	})
}

func TestRemoveReportsChangedOutcomes(t *testing.T) {
	selections := []Selection{{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}}}}
	var removed []string
	hosts := map[string]Host{
		"web-1": &scriptedHost{
			check: passingCheck,
			remove: func(_ context.Context, workloadName string) (host.Result, error) {
				removed = append(removed, workloadName)

				return host.Result{Changed: true}, nil
			},
		},
	}
	var events []string
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	results, err := group.Remove(t.Context(), selections)
	if err != nil {
		t.Fatal(err)
	}
	assertStates(t, results[0], []string{TargetCompleted})
	if !results[0].Targets[0].Changed || results[0].Targets[0].Container != "" {
		t.Fatalf("target result = %+v", results[0].Targets[0])
	}
	assertStrings(t, removed, []string{"storefront"})
}

func TestStatusCollectsAllTargetsBestEffort(t *testing.T) {
	selections := []Selection{
		{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}, {Zone: "b", Instance: "web-2"}, {Zone: "c", Instance: "web-3"}}},
		{Workload: "jobs", Targets: []Target{{Zone: "d", Instance: "jobs-1"}}},
	}
	observed := host.Observation{
		State:      host.StateCurrent,
		Containers: []host.Container{{ID: "container-1", Name: "web", Image: "sha256:image", Running: true, Healthy: true}},
		Route:      &host.Route{Address: "web:8080", ContainerID: "container-1"},
	}
	observeFailure := errors.New("route lookup failed")
	// The status hosts set no check function: status never preflights and
	// panics if it calls Check.
	hosts := map[string]Host{
		"web-1": &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
			return observed, nil
		}},
		"web-2": &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
			return host.Observation{State: host.StateCurrent, Containers: observed.Containers}, observeFailure
		}},
		"jobs-1": &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
			return host.Observation{State: host.StateMissing}, nil
		}},
	}
	var events []string
	var facts []host.Progress
	group, err := NewGroup(hostProvider(&events, hosts, nil), func(fact host.Progress) { facts = append(facts, fact) }, Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := group.Status(t.Context(), selections)
	if err == nil || !strings.Contains(err.Error(), "web-2") || !strings.Contains(err.Error(), "web-3") {
		t.Fatalf("error = %v", err)
	}
	if len(statuses) != 2 || statuses[0].Workload != "storefront" || statuses[1].Workload != "jobs" {
		t.Fatalf("statuses = %+v", statuses)
	}
	first := statuses[0].Targets
	if len(first) != 3 {
		t.Fatalf("targets = %+v", first)
	}
	if !reflect.DeepEqual(first[0].Observation, observed) || first[0].Error != nil {
		t.Fatalf("observed target = %+v", first[0])
	}
	// A partially classified observation keeps its own state and detail;
	// only the failing part is reported as an error.
	if first[1].Observation.State != host.StateCurrent || first[1].Observation.Containers == nil || first[1].Error == nil {
		t.Fatalf("failed observation = %+v", first[1])
	}
	if !errors.Is(first[1].Error, observeFailure) {
		t.Fatalf("failed observation error = %v", first[1].Error)
	}
	if first[2].Observation.State != StateUnreachable || first[2].Error == nil || !strings.Contains(first[2].Error.Error(), "IAP unavailable") {
		t.Fatalf("unreachable target = %+v", first[2])
	}
	second := statuses[1].Targets
	if len(second) != 1 || second[0].Observation.State != host.StateMissing || second[0].Error != nil {
		t.Fatalf("second workload targets = %+v", second)
	}
	// Status is observation only: it emits no progress.
	if len(facts) != 0 {
		t.Fatalf("status emitted progress: %+v", facts)
	}
}

func TestStatusKeepsObservationWhenOnlyCloseFails(t *testing.T) {
	// A local cleanup failure never discards a successful observation:
	// the state, containers, and route stand, and the close error is
	// reported on the target and in the aggregate error.
	closeFailure := errors.New("close failed")
	observed := host.Observation{
		State:      host.StateCurrent,
		Containers: []host.Container{{ID: "container-1", Running: true, Healthy: true}},
		Route:      &host.Route{Address: "web:8080", ContainerID: "container-1"},
	}
	provider := &stubProvider{open: func(context.Context, Target, host.Reporter) (Handle, error) {
		return Handle{
			Host: &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
				return observed, nil
			}},
			Close: func() error { return closeFailure },
		}, nil
	}}
	group, err := NewGroup(provider, silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := group.Status(t.Context(), []Selection{{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}}}})
	if !errors.Is(err, closeFailure) {
		t.Fatalf("aggregate error = %v", err)
	}
	target := statuses[0].Targets[0]
	if target.Observation.State != host.StateCurrent || target.Observation.Containers == nil || target.Observation.Route == nil {
		t.Fatalf("observation discarded on close failure: %+v", target)
	}
	if !errors.Is(target.Error, closeFailure) {
		t.Fatalf("target error = %v", target.Error)
	}
}

func TestStatusMarksUnclassifiedObservationUnreachable(t *testing.T) {
	// A host failure that provides no state at all leaves the target
	// genuinely unreachable: nothing is invented.
	observeFailure := errors.New("docker daemon unreachable")
	provider := &stubProvider{open: func(context.Context, Target, host.Reporter) (Handle, error) {
		return Handle{
			Host: &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
				return host.Observation{}, observeFailure
			}},
			Close: func() error { return nil },
		}, nil
	}}
	group, err := NewGroup(provider, silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := group.Status(t.Context(), []Selection{{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}}}})
	if !errors.Is(err, observeFailure) {
		t.Fatalf("aggregate error = %v", err)
	}
	target := statuses[0].Targets[0]
	if target.Observation.State != StateUnreachable || !errors.Is(target.Error, observeFailure) {
		t.Fatalf("target = %+v", target)
	}
}

func TestStatusMarksRemainingTargetsUnreachableAfterCancellation(t *testing.T) {
	selections := []Selection{{Workload: "storefront", Targets: []Target{{Zone: "a", Instance: "web-1"}, {Zone: "b", Instance: "web-2"}}}}
	ctx, cancel := context.WithCancel(context.Background())
	hosts := map[string]Host{
		"web-1": &scriptedHost{status: func(context.Context, string) (host.Observation, error) {
			cancel()

			return host.Observation{State: host.StateCurrent}, nil
		}},
	}
	var events []string
	group, err := NewGroup(hostProvider(&events, hosts, nil), silentReporter(), Config{PreflightTimeout: testPreflightTimeout})
	if err != nil {
		t.Fatal(err)
	}

	statuses, err := group.Status(ctx, selections)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	targets := statuses[0].Targets
	if len(targets) != 2 {
		t.Fatalf("targets = %+v", targets)
	}
	if targets[0].Observation.State != host.StateCurrent || targets[0].Error != nil {
		t.Fatalf("observed target = %+v", targets[0])
	}
	if targets[1].Observation.State != StateUnreachable || !errors.Is(targets[1].Error, context.Canceled) {
		t.Fatalf("cancelled target = %+v", targets[1])
	}
}

// passingCheck accepts any preflight check.
func passingCheck(context.Context, string, bool) error {
	return nil
}

// stubProvider scripts the provider seam in group tests.
type stubProvider struct {
	open func(context.Context, Target, host.Reporter) (Handle, error)
}

func (p *stubProvider) Open(ctx context.Context, target Target, report host.Reporter) (Handle, error) {
	return p.open(ctx, target, report)
}

// hostProvider opens the scripted host for each target instance and records
// open and close events. An instance without a host fails to open.
func hostProvider(events *[]string, hosts map[string]Host, closeErrs map[string]error) *stubProvider {
	return &stubProvider{open: func(_ context.Context, target Target, _ host.Reporter) (Handle, error) {
		*events = append(*events, "connect-"+target.Instance)
		targetHost, found := hosts[target.Instance]
		if !found {
			return Handle{}, errors.New("IAP unavailable")
		}

		return Handle{Host: targetHost, Close: func() error {
			*events = append(*events, "close-"+target.Instance)

			return closeErrs[target.Instance]
		}}, nil
	}}
}

// scriptedHost scripts high-level host results for group tests. Unset
// functions panic when called, proving the group never reaches them.
type scriptedHost struct {
	check   func(context.Context, string, bool) error
	deploy  func(context.Context, workload.Definition) (host.Result, error)
	status  func(context.Context, string) (host.Observation, error)
	restart func(context.Context, string) (host.Result, error)
	remove  func(context.Context, string) (host.Result, error)
}

func (h *scriptedHost) Check(ctx context.Context, workloadName string, desiredHTTP bool) error {
	if h.check == nil {
		panic("scriptedHost.check: not set")
	}

	return h.check(ctx, workloadName, desiredHTTP)
}

func (h *scriptedHost) Deploy(ctx context.Context, definition workload.Definition) (host.Result, error) {
	if h.deploy == nil {
		panic("scriptedHost.deploy: not set")
	}

	return h.deploy(ctx, definition)
}

func (h *scriptedHost) Status(ctx context.Context, workloadName string) (host.Observation, error) {
	if h.status == nil {
		panic("scriptedHost.status: not set")
	}

	return h.status(ctx, workloadName)
}

func (h *scriptedHost) Restart(ctx context.Context, workloadName string) (host.Result, error) {
	if h.restart == nil {
		panic("scriptedHost.restart: not set")
	}

	return h.restart(ctx, workloadName)
}

func (h *scriptedHost) Remove(ctx context.Context, workloadName string) (host.Result, error) {
	if h.remove == nil {
		panic("scriptedHost.remove: not set")
	}

	return h.remove(ctx, workloadName)
}

// recordingHost counts the calls made on one scripted host.
type recordingHost struct {
	checks  int
	deploys int
}

// script builds a host whose check and deploy calls are counted; any other
// call panics.
func (r *recordingHost) script(check func(context.Context, string, bool) error) *scriptedHost {
	return &scriptedHost{
		check: func(ctx context.Context, workloadName string, desiredHTTP bool) error {
			r.checks++

			return check(ctx, workloadName, desiredHTTP)
		},
		deploy: func(context.Context, workload.Definition) (host.Result, error) {
			r.deploys++

			return host.Result{Changed: true}, nil
		},
	}
}

func storefrontDefinition() workload.Definition {
	return workload.Definition{
		Name: "storefront",
		GCP: workload.GCP{
			Project:   "acme-prod",
			Instances: []workload.Instance{{Zone: "us-central1-a", Name: "web-1"}},
		},
		Container: workload.ContainerConfig{
			Image:       "docker.io/example/web:v1",
			Command:     []string{"bin/web"},
			Environment: map[string]string{"APP_ENV": "production"},
		},
	}
}

func assertStates(t *testing.T, result WorkloadResult, want []string) {
	t.Helper()
	if len(result.Targets) != len(want) {
		t.Fatalf("target results = %+v, want states %v", result.Targets, want)
	}
	for index, state := range want {
		if result.Targets[index].State != state {
			t.Fatalf("target %d state = %q, want %q (results %+v)", index, result.Targets[index].State, state, result.Targets)
		}
	}
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func assertBools(t *testing.T, got, want []bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
