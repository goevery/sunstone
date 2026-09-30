package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/goevery/sunstone/internal/modules/deployment"
	"github.com/urfave/cli/v3"
)

type moduleFactory func(io.Writer) (deployment.Module, error)

func main() {
	if err := newCommand().Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return commandWith(deployment.New)
}

func commandWith(newModule moduleFactory) *cli.Command {
	return &cli.Command{
		Name:        "sunstone",
		Usage:       "Deploy containers to Google Cloud VMs",
		UsageText:   "sunstone COMMAND [OPTIONS]",
		Description: "Deploy and operate container workloads on Google Cloud VMs without a control plane. Each YAML document defines one workload.",
		Commands: []*cli.Command{
			deployCommand(newModule),
			workloadCommand(newModule, "status", "Show workload status", "Report the image, container, and health state of each workload on every configured VM."),
			workloadCommand(newModule, "restart", "Restart workloads", "Restart each workload one VM at a time using its normal deployment strategy."),
			workloadCommand(newModule, "remove", "Remove workloads", "Remove each workload from its configured VMs. HTTP traffic is drained first. VMs and Sunbeam proxies remain running."),
		},
	}
}

func deployCommand(newModule moduleFactory) *cli.Command {
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
			module, err := newModule(cmd.Writer)
			if err != nil {
				return err
			}
			_, err = module.Deploy(ctx, deployment.Request{Filename: cmd.String("filename"), ImpersonateServiceAccount: cmd.String("impersonate-service-account")})
			return err
		},
	}
}

func workloadCommand(newModule moduleFactory, name, usage, description string) *cli.Command {
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
			module, err := newModule(cmd.Writer)
			if err != nil {
				return err
			}
			request := deployment.WorkloadRequest{Filenames: cmd.StringSlice("filename"), ImpersonateServiceAccount: cmd.String("impersonate-service-account")}
			switch cmd.Name {
			case "status":
				result, statusErr := module.Status(ctx, request)
				if err := writeStatus(cmd.Writer, result); err != nil {
					return err
				}
				return statusErr
			case "restart":
				_, err = module.Restart(ctx, request)
				return err
			case "remove":
				_, err = module.Remove(ctx, request)
				return err
			default:
				return fmt.Errorf("unsupported operation %s", cmd.Name)
			}
		},
	}
}

func writeStatus(writer io.Writer, result deployment.StatusResult) error {
	for _, workload := range result.Workloads {
		for _, instance := range workload.Instances {
			if _, err := fmt.Fprintf(writer, "%s / %s (%s): %s\n", workload.Workload, instance.Instance, instance.Zone, instance.State); err != nil {
				return err
			}
			for _, container := range instance.Containers {
				if _, err := fmt.Fprintf(writer, "  container: %s name=%s image=%s running=%t healthy=%t\n", container.ID, container.Name, container.Image, container.Running, container.Healthy); err != nil {
					return err
				}
			}
			if instance.Route != nil {
				if _, err := fmt.Fprintf(writer, "  route: %s -> %s\n", instance.Route.Address, instance.Route.ContainerID); err != nil {
					return err
				}
			}
			if instance.Error != "" {
				if _, err := fmt.Fprintf(writer, "  error: %s\n", instance.Error); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
