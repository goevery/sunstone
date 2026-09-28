package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

func main() {
	if err := newCommand().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:        "sunstone",
		Usage:       "Deploy containers to Google Cloud VMs",
		UsageText:   "sunstone COMMAND [OPTIONS]",
		Description: "Deploy and operate container workloads on Google Cloud VMs without a control plane. Each YAML document defines one workload.",
		Commands: []*cli.Command{
			workloadCommand(
				"deploy",
				"Deploy workloads",
				"Validate workload definitions and deploy them to their configured VMs. HTTP workloads use zero-downtime rolling deployment. Background workloads use stop-then-start replacement.",
			),
			workloadCommand(
				"status",
				"Show workload status",
				"Report the image, container, and health state of each workload on every configured VM.",
			),
			workloadCommand(
				"restart",
				"Restart workloads",
				"Restart each workload one VM at a time using its normal deployment strategy.",
			),
			workloadCommand(
				"remove",
				"Remove workloads",
				"Remove each workload from its configured VMs. HTTP traffic is drained first. VMs and Sunbeam proxies remain running.",
			),
		},
	}
}

func workloadCommand(name, usage, description string) *cli.Command {
	return &cli.Command{
		Name:        name,
		Usage:       usage,
		UsageText:   fmt.Sprintf("sunstone %s -f FILE_OR_DIRECTORY [-f FILE_OR_DIRECTORY ...]", name),
		Description: description,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:      "filename",
				Aliases:   []string{"f"},
				Usage:     "load workloads from `FILE_OR_DIRECTORY`",
				Required:  true,
				TakesFile: true,
			},
		},
		Action: func(_ context.Context, cmd *cli.Command) error {
			_, err := fmt.Fprintf(cmd.Writer, "%s is not implemented yet\n", cmd.Name)
			return err
		},
	}
}
