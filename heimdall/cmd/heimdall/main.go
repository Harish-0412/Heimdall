// Command heimdall is the developer-facing CLI for Heimdall preview
// environments.
package main

import (
	"os"

	"github.com/heimdall-dev/heimdall/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
