package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/redact"
	"github.com/heimdall-dev/heimdall/internal/render"
)

const diagnoseUsage = `Usage: heimdall diagnose --state file --allow-context name [flags]
       heimdall diagnose --snapshot file [flags]

Explain why a preview failed and what to do about it: the most likely root
cause first, with a stable code (docs/diagnostics.md), a specific summary, a
suggestion and redacted evidence.

The first form inspects the live preview recorded in --state; the second
diagnoses a snapshot saved earlier with --save-snapshot (for bug reports:
snapshots hold no secret values).

Exit codes: 0 no problems found, 1 problems found, 2 could not diagnose.

Flags:
`

func runDiagnose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, diagnoseUsage)
		fs.PrintDefaults()
	}
	state := fs.String("state", ".heimdall/environment.json", "local desired-state file of the preview")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default Kubernetes loading rules)")
	kubeContext := fs.String("context", "", "kube-context (default the one saved in --state)")
	var allow listFlag
	fs.Var(&allow, "allow-context", "exact trusted kube-context; repeatable and required with --state")
	snapshot := fs.String("snapshot", "", "diagnose a saved snapshot instead of the live preview")
	save := fs.String("save-snapshot", "", "also write the captured snapshot (JSON, redacted) to this file")
	format := fs.String("format", "text", "output format: text, json or markdown (a pull-request comment)")
	positional, parseErr := parseInterspersed(fs, args)
	if parseErr != nil {
		if errors.Is(parseErr, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if len(positional) > 0 || (*format != "text" && *format != "json" && *format != "markdown") {
		fs.Usage()
		return ExitUsage
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "heimdall:", err); return ExitUsage }

	var snap *diagnose.Snapshot
	var commit string
	if *snapshot != "" {
		b, err := os.ReadFile(*snapshot)
		if err != nil {
			return fail(err)
		}
		snap = &diagnose.Snapshot{}
		if err := json.Unmarshal(b, snap); err != nil {
			return fail(fmt.Errorf("%s: %w", *snapshot, err))
		}
		if snap.Version != diagnose.SnapshotVersion {
			return fail(fmt.Errorf("%s: snapshot version %d is not supported", *snapshot, snap.Version))
		}
		// A snapshot supplied by hand may not have gone through Collect.
		// Apply the same structural stripping and pattern safety net before
		// showing or copying any of it.
		snap.Sanitize(redact.New())
	} else {
		local, err := loadLocal(*state)
		if err != nil {
			return fail(err)
		}
		if *kubeContext == "" {
			*kubeContext = local.KubeContext
		}
		restConfig, selected, err := clusterConfig(*kubeconfig, *kubeContext, allow)
		if err != nil {
			return fail(err)
		}
		cluster, err := clusterIdentity(restConfig)
		if err != nil {
			return fail(err)
		}
		if local.KubeContext != selected || local.Cluster != cluster {
			return fail(fmt.Errorf("engine.cluster_mismatch: saved intent belongs to a different cluster/context"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		snap, err = collectFor(ctx, restConfig, local.Context, nil)
		if err != nil {
			return fail(err)
		}
		commit = local.Context.SHA
	}
	if *save != "" {
		b, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return fail(err)
		}
		if err := os.WriteFile(*save, append(b, '\n'), 0o600); err != nil {
			return fail(err)
		}
	}
	report := diagnose.Diagnose(snap)
	var err error
	switch *format {
	case "json":
		err = diagnose.WriteJSON(stdout, report)
	case "markdown":
		err = diagnose.WriteMarkdown(stdout, report, diagnose.CommentContext{Environment: snap.Namespace, Generation: snap.Generation, Commit: commit})
	default:
		err = diagnose.WriteText(stdout, report)
		for _, n := range snap.Notes {
			fmt.Fprintln(stderr, "note:", n)
		}
	}
	if err != nil {
		return fail(err)
	}
	if len(report.Diagnoses) > 0 {
		return ExitInvalid
	}
	return ExitOK
}

// collectFor captures the snapshot of the preview a render context names,
// including its routes.
func collectFor(ctx context.Context, rc *rest.Config, rctx render.Context, failure *diagnose.Failure) (*diagnose.Snapshot, error) {
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return diagnose.Collect(ctx, client, diagnose.Options{
		Namespace:  render.NamespaceFor(rctx.Repo, rctx.PR, rctx.URLSuffix),
		Generation: rctx.Generation,
		Failure:    failure,
		Dynamic:    dyn,
	})
}

// explainFailure prints the diagnosis of a failed up or reset, and saves its
// snapshot when asked. It never changes the command's outcome: diagnosis is
// best effort. A failure before the preview could be inspected (an import
// that is not approved, a namespace that is gone) is still explained from
// the failure itself.
func explainFailure(rc *rest.Config, rctx render.Context, opErr error, save string, stderr io.Writer) {
	var e *engine.Error
	failure := &diagnose.Failure{Code: "engine.cluster", Message: opErr.Error()}
	if errors.As(opErr, &e) {
		failure.Code, failure.Message = e.Code, e.Message
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := collectFor(ctx, rc, rctx, failure)
	if err != nil {
		snap = diagnose.ForFailure(render.NamespaceFor(rctx.Repo, rctx.PR, rctx.URLSuffix), rctx.Generation, failure, err)
	}
	if save != "" {
		if b, err := json.MarshalIndent(snap, "", "  "); err == nil {
			if err := os.WriteFile(save, append(b, '\n'), 0o600); err != nil {
				fmt.Fprintln(stderr, "heimdall: cannot save the snapshot:", err)
			}
		}
	}
	fmt.Fprintln(stderr)
	_ = diagnose.WriteText(stderr, diagnose.Diagnose(snap))
	for _, n := range snap.Notes {
		fmt.Fprintln(stderr, "note:", n)
	}
}
