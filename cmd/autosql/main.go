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
		if err := flags.Parse(args[1:]); err != nil {
			if err == flag.ErrHelp {
				return
			}
			_, _ = os.Stderr.WriteString("autosql: operator: invalid flags\n")
			os.Exit(2)
		}
		if err := operatorcontroller.Run(ctx, *leaderElection); err != nil {
			_, _ = os.Stderr.WriteString("autosql: operator: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}
	services, err := cli.ProductionServices()
	if err != nil {
		_, _ = os.Stderr.WriteString("autosql: config: production apply configuration invalid\n")
		os.Exit(3)
	}
	os.Exit(cli.RunWithServices(ctx, args, cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, services))
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
