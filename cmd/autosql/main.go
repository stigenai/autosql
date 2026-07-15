package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"autosql/internal/cli"
	"autosql/internal/operatorcontroller"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "operator" {
		flags := flag.NewFlagSet("operator", flag.ContinueOnError)
		leaderElection := flags.Bool("leader-election", true, "enable Kubernetes lease leader election")
		if err := flags.Parse(os.Args[2:]); err != nil {
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
	os.Exit(cli.RunWithServices(ctx, os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, services))
}
