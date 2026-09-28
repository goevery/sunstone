package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goevery/sunstone/internal/modules/deployment"
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
			deployCommand(),
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

func deployCommand() *cli.Command {
	return &cli.Command{
		Name:        "deploy",
		Usage:       "Deploy a background workload",
		UsageText:   "sunstone deploy -f FILE --os-login-user EMAIL",
		Description: "Validate one background workload definition and deploy it to its configured VM.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:      "filename",
				Aliases:   []string{"f"},
				Usage:     "load a workload from `FILE`",
				Required:  true,
				TakesFile: true,
			},
			&cli.StringFlag{
				Name:     "os-login-user",
				Usage:    "authenticate SSH as the OS Login identity `EMAIL`",
				Required: true,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			module, err := deployment.New(cmd.Writer)
			if err != nil {
				return err
			}
			_, err = module.Deploy(ctx, deployment.Request{
				Filename:    cmd.String("filename"),
				OSLoginUser: cmd.String("os-login-user"),
			})
			return err
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
