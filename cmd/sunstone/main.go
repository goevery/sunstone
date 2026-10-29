package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	progress "github.com/goevery/sunstone/internal/cli"
	"github.com/goevery/sunstone/internal/deployment"
	"github.com/goevery/sunstone/internal/workload"
)

// preflightTimeout bounds each target's preflight check during mutations;
// the composition root chooses the value.
const preflightTimeout = 30 * time.Second

// commandProvider is the command-owned host provider: it opens target hosts
// for the group and releases every command-scoped session when the command
// exits. Close is part of the seam, never an optional type assertion.
type commandProvider interface {
	deployment.Provider
	Close() error
}

// providerFactory builds the command-scoped host provider for one command
// run, configured with the identity to impersonate.
type providerFactory func(serviceAccount string) (commandProvider, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newCommand().Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return commandWith(productionProvider)
}

func commandWith(newProvider providerFactory) *cli.Command {
	return &cli.Command{
		Name:        "sunstone",
		Usage:       "Deploy containers to Google Cloud VMs",
		UsageText:   "sunstone COMMAND [OPTIONS]",
		Description: "Deploy and operate container workloads on Google Cloud VMs without a control plane. Each YAML document defines one workload.",
		Commands: []*cli.Command{
			deployCommand(newProvider),
			workloadCommand(newProvider, "status", "Show workload status", "Report the image, container, and health state of each workload on every configured VM."),
			workloadCommand(newProvider, "restart", "Restart workloads", "Restart each workload one VM at a time using its normal deployment strategy."),
			workloadCommand(newProvider, "remove", "Remove workloads", "Remove each workload from its configured VMs. HTTP traffic is drained first. VMs and Sunbeam proxies remain running."),
		},
	}
}

func deployCommand(newProvider providerFactory) *cli.Command {
	return &cli.Command{
		Name:        "deploy",
		Usage:       "Deploy a workload",
		UsageText:   "sunstone deploy -f FILE --impersonate-service-account EMAIL",
		Description: "Validate one workload definition and deploy it to its configured VMs.",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "filename", Aliases: []string{"f"}, Usage: "load a workload from `FILE`", Required: true, TakesFile: true},
			&cli.StringFlag{Name: "impersonate-service-account", Usage: "deploy as the service account `EMAIL`", Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			formatter, err := progress.NewFormatter(cmd.Writer)
			if err != nil {
				return err
			}
			// Configuration loads and validates before any connection
			// resources exist; malformed files never reach the cloud.
			definition, err := workload.NewLoader().Load(cmd.String("filename"))
			if err != nil {
				return err
			}
			provider, err := newProvider(cmd.String("impersonate-service-account"))
			if err != nil {
				return err
			}
			group, err := deployment.NewGroup(provider, formatter.Report, deployment.Config{PreflightTimeout: preflightTimeout})
			if err != nil {
				return errors.Join(err, provider.Close())
			}

			result, operationErr := group.Deploy(ctx, definition)
			// Results render even when the operation failed; the display
			// failure is labeled so it never reads as a workload failure.
			formatter.WriteResults([]deployment.WorkloadResult{result})

			return errors.Join(operationErr, displayError(formatter.Err()), provider.Close())
		},
	}
}

func workloadCommand(newProvider providerFactory, name, usage, description string) *cli.Command {
	return &cli.Command{
		Name:        name,
		Usage:       usage,
		UsageText:   fmt.Sprintf("sunstone %s -f FILE_OR_DIRECTORY [-f FILE_OR_DIRECTORY ...] --impersonate-service-account EMAIL", name),
		Description: description,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "filename", Aliases: []string{"f"}, Usage: "load workloads from `FILE_OR_DIRECTORY`", Required: true, TakesFile: true},
			&cli.StringFlag{Name: "impersonate-service-account", Usage: "operate as the service account `EMAIL`", Required: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			formatter, err := progress.NewFormatter(cmd.Writer)
			if err != nil {
				return err
			}
			// Configuration loads and validates before any connection
			// resources exist; malformed files never reach the cloud.
			definitions, err := workload.NewLoader().LoadAll(cmd.StringSlice("filename"))
			if err != nil {
				return err
			}
			provider, err := newProvider(cmd.String("impersonate-service-account"))
			if err != nil {
				return err
			}
			group, err := deployment.NewGroup(provider, formatter.Report, deployment.Config{PreflightTimeout: preflightTimeout})
			if err != nil {
				return errors.Join(err, provider.Close())
			}

			selections := selectionsOf(definitions)
			switch cmd.Name {
			case "status":
				statuses, statusErr := group.Status(ctx, selections)
				// Observations render even when targets failed; the display
				// failure is labeled so it never reads as a workload failure.
				formatter.WriteStatus(statuses)

				return errors.Join(statusErr, displayError(formatter.Err()), provider.Close())
			case "restart":
				results, operationErr := group.Restart(ctx, selections)
				formatter.WriteResults(results)

				return errors.Join(operationErr, displayError(formatter.Err()), provider.Close())
			case "remove":
				results, operationErr := group.Remove(ctx, selections)
				formatter.WriteResults(results)

				return errors.Join(operationErr, displayError(formatter.Err()), provider.Close())
			default:
				return errors.Join(fmt.Errorf("unsupported operation %s", cmd.Name), provider.Close())
			}
		},
	}
}

// displayError labels an output failure so it stays distinguishable from
// workload failures in the joined command error.
func displayError(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("write output: %w", err)
}

// selectionsOf derives the name-only selections for the loaded definitions.
func selectionsOf(definitions []workload.Definition) []deployment.Selection {
	selections := make([]deployment.Selection, len(definitions))
	for index, definition := range definitions {
		selections[index] = deployment.SelectionOf(definition)
	}

	return selections
}
