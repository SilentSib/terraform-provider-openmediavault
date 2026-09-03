// Terraform Provider for OpenMediaVault.
//
// Requires OpenMediaVault 8 or newer (enforced at runtime in
// internal/provider.Configure).
package main

import (
	"context"
	"flag"
	"log"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/example/terraform-provider-openmediavault/internal/provider"
)

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=x.y.z"
//
// goreleaser does this automatically on tagged releases; it stays "dev"
// for local builds.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// Address must match the "source" configured for this provider
		// in Terraform configurations (registry.terraform.io/<namespace>/openmediavault).
		// Update the namespace below once this provider is published.
		Address: "registry.terraform.io/SilentSib/openmediavault",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
