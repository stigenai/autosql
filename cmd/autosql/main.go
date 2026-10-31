package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"autosql/internal/cli"
	"autosql/internal/operatorcontroller"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[1:]
	if operatorServerCommand(args) {
		flags := flag.NewFlagSet("operator", flag.ContinueOnError)
		leaderElection := flags.Bool("leader-election", true, "enable Kubernetes lease leader election")
		metricsAddress := flags.String("metrics-bind-address", ":8080", "internal metrics address (0 disables)")
		healthAddress := flags.String("health-probe-bind-address", ":8081", "internal health and readiness address (0 disables)")
		if err := flags.Parse(args[1:]); err != nil {
			if err == flag.ErrHelp {
				return
			}
			_, _ = os.Stderr.WriteString("autosql: operator: invalid flags\n")
			os.Exit(2)
		}
		if err := operatorcontroller.RunWithOptions(ctx, operatorcontroller.RunOptions{LeaderElection: *leaderElection, MetricsBindAddress: *metricsAddress, HealthProbeBindAddress: *healthAddress}); err != nil {
			_, _ = os.Stderr.WriteString("autosql: operator: startup or manager failure; check operator configuration and cluster access\n")
			os.Exit(1)
		}
		return
	}
	// These commands own their read, generation, or development services.
	// Constructing Apply services would resolve unrelated production credentials.
	if localReadCommand(args) || developmentCommand(args) {
		os.Exit(cli.Run(ctx, args, cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
	}
	services, err := cli.ProductionServices()
	if err != nil {
		_, _ = os.Stderr.WriteString("autosql: config: production apply configuration invalid\n")
		os.Exit(3)
	}
	os.Exit(cli.RunWithServices(ctx, args, cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, services))
}

func localReadCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "plan" {
		return len(args) == 1 || strings.HasPrefix(args[1], "-")
	}
	if len(args) >= 2 && args[0] == "migrate" && args[1] == "generate" {
		return true
	}
	return len(args) >= 2 && ((args[0] == "config" && args[1] == "validate") || (args[0] == "schema" && args[1] == "load"))
}

// operatorServerCommand keeps the historical `autosql operator [flags]`
// controller entrypoint while reserving operator subcommand namespaces for
// the normal CLI dispatcher. The standard library flag parser stops at the
// first positional argument, so relying on Parse to reject these silently
// launched the controller instead of running the requested command.
func operatorServerCommand(args []string) bool {
	if len(args) == 0 || args[0] != "operator" {
		return false
	}
	return len(args) == 1 || strings.HasPrefix(args[1], "-")
}

func developmentCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return args[0] == "database" && args[1] == "dev" || (args[0] == "schema" || args[0] == "migrate") && args[1] == "test"
}
