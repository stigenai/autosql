package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"autosql/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	services, err := cli.ProductionServices()
	if err != nil {
		_, _ = os.Stderr.WriteString("autosql: config: production apply configuration invalid\n")
		os.Exit(3)
	}
	os.Exit(cli.RunWithServices(ctx, os.Args[1:], cli.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, services))
}
