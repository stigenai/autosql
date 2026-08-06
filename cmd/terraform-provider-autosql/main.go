package main

import (
	"context"
	"flag"
	"log"

	"autosql/internal/cli"
	"autosql/internal/terraformprovider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

func main() {
	debug := flag.Bool("debug", false, "run the provider with debugger support")
	flag.Parse()
	err := providerserver.Serve(context.Background(), terraformprovider.New(cli.Version), providerserver.ServeOpts{
		Address: "registry.terraform.io/stigenai/autosql",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
