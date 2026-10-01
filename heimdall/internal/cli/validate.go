package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/heimdall-dev/heimdall/internal/config"
)

const defaultConfigFile = "heimdall.yaml"

type validateReport struct {
	File        string             `json:"file"`
	Valid       bool               `json:"valid"`
	Errors      int                `json:"errors"`
	Warnings    int                `json:"warnings"`
	Diagnostics config.Diagnostics `json:"diagnostics"`
}

func runValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: heimdall validate [flags] [file]")
		fs.PrintDefaults()
	}
	format := fs.String("format", "text", "output format: text or json")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	baseline := fs.String("baseline", "", "trusted config from the default branch; fail if the file loosens it")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "heimdall: unknown --format %q (want text or json)\n", *format)
		return ExitUsage
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "heimdall: validate takes at most one file")
		return ExitUsage
	}
	file := defaultConfigFile
	if len(pos) == 1 {
		file = pos[0]
	}

	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: cannot read config: %v\n", err)
		return ExitUsage
	}
	defer f.Close()

	policy := config.DefaultPolicy()
	cfg, diags := config.Load(f, policy)

	// Trust check: a PR config may not loosen the protected default branch's
	// config. The baseline itself must be valid; if it is not, we cannot judge.
	if *baseline != "" && cfg != nil {
		base, code := loadBaseline(*baseline, policy, stderr)
		if base == nil {
			return code
		}
		diags = append(diags, config.CompareToBaseline(base, cfg)...)
		diags.Sort()
	}
	failed := diags.HasErrors() || (*strict && diags.Warnings() > 0)

	if *format == "json" {
		if diags == nil {
			diags = config.Diagnostics{} // render [] rather than null
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(validateReport{
			File: file, Valid: !failed, Errors: diags.Errors(), Warnings: diags.Warnings(), Diagnostics: diags,
		}); err != nil {
			fmt.Fprintf(stderr, "heimdall: %v\n", err)
			return ExitUsage
		}
	} else {
		writeText(stderr, file, diags)
		if failed {
			fmt.Fprintf(stderr, "%s: %s\n", file, counts(diags))
		} else {
			fmt.Fprintf(stdout, "%s: valid (%s)\n", file, describe(cfg))
		}
	}

	if failed {
		return ExitInvalid
	}
	return ExitOK
}

func loadBaseline(path string, policy config.Policy, stderr io.Writer) (*config.Config, int) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "heimdall: cannot read baseline: %v\n", err)
		return nil, ExitUsage
	}
	defer f.Close()
	base, diags := config.Load(f, policy)
	if base == nil {
		writeText(stderr, path, diags)
		fmt.Fprintf(stderr, "heimdall: baseline %s is itself invalid; fix the default branch first\n", path)
		return nil, ExitUsage
	}
	return base, ExitOK
}

// parseInterspersed lets flags appear after the positional argument
// ("validate heimdall.yaml --strict"), which the standard flag package does not.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func writeText(w io.Writer, file string, diags config.Diagnostics) {
	for _, d := range diags {
		loc := file
		if d.Line > 0 {
			loc = fmt.Sprintf("%s:%d", file, d.Line)
			if d.Column > 0 {
				loc = fmt.Sprintf("%s:%d", loc, d.Column)
			}
		}
		fmt.Fprintf(w, "%s: %s[%s]: %s\n", loc, d.Severity, d.Code, d.Message)
		if d.Hint != "" {
			fmt.Fprintf(w, "    hint: %s\n", d.Hint)
		}
	}
}

func counts(d config.Diagnostics) string {
	return fmt.Sprintf("%s, %s", plural(d.Errors(), "error"), plural(d.Warnings(), "warning"))
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func describe(c *config.Config) string {
	parts := []string{plural(len(c.Services), "service"), plural(len(c.Workers), "worker")}
	if deps := c.EnabledDependencies(); len(deps) > 0 {
		parts = append(parts, "dependencies: "+strings.Join(deps, ", "))
	}
	return strings.Join(parts, ", ")
}
