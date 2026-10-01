// Package cli implements the heimdall command line. Run is a plain function
// over streams and returns an exit code, so every command is unit-testable
// without spawning a process.
package cli

import (
	"fmt"
	"io"

	"github.com/heimdall-dev/heimdall/internal/version"
)

// Exit codes. Scripts and CI rely on telling "your config is wrong" (1) apart
// from "the tool was misused or could not run" (2).
const (
	ExitOK      = 0
	ExitInvalid = 1
	ExitUsage   = 2
)

const usageText = `Heimdall - secure, disposable full-stack environments for every pull request.

Usage:
  heimdall <command> [arguments]

Commands:
  validate [file]   Check a heimdall.yaml (default: ./heimdall.yaml)
  render [file]     Print the Kubernetes objects for a preview, stage by stage
  schema            Print the JSON Schema for heimdall.yaml (editor support)
  version           Print version information
  help              Show this help

Run "heimdall <command> -h" for command flags.
`

// Run executes the CLI with args (excluding the program name) and returns the
// process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "render":
		return runRender(args[1:], stdout, stderr)
	case "schema":
		return runSchema(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "heimdall %s (commit %s)\n", version.Version, version.Commit)
		return ExitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "heimdall: unknown command %q\n\n%s", args[0], usageText)
		return ExitUsage
	}
}
