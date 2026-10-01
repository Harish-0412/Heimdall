package cli

import (
	"fmt"
	"io"

	"github.com/heimdall-dev/heimdall/internal/config"
)

func runSchema(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "--help" {
			fmt.Fprintf(stdout, "Usage: heimdall schema\n\nPrints the JSON Schema for heimdall.yaml. Point your editor at it:\n  # yaml-language-server: $schema=%s\n", config.SchemaID)
			return ExitOK
		}
		fmt.Fprintln(stderr, "heimdall: schema takes no arguments")
		return ExitUsage
	}
	out, err := config.JSONSchema()
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: %v\n", err)
		return ExitUsage
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "heimdall: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}
