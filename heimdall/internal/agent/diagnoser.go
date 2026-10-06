package agent

import (
	"context"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/heimdall-dev/heimdall/internal/diagnose"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// clusterDiagnoser explains failed operations from a snapshot of the preview
// namespace and its routes (the agent reads them through its per-namespace
// role).
type clusterDiagnoser struct {
	client  kubernetes.Interface
	dynamic dynamic.Interface
}

func (d clusterDiagnoser) Diagnose(ctx context.Context, spec engine.Spec, f diagnose.Failure) (*diagnose.Report, error) {
	c := spec.Context
	ns := render.NamespaceFor(c.Repo, c.PR, c.URLSuffix)
	snap, err := diagnose.Collect(ctx, d.client, diagnose.Options{Namespace: ns, Generation: c.Generation, Failure: &f, Dynamic: d.dynamic})
	if err != nil {
		// Nothing to inspect (the failure came before the namespace, or it
		// cannot be read): the failure alone still gets its diagnosis.
		snap = diagnose.ForFailure(ns, c.Generation, &f, err)
	}
	return diagnose.Diagnose(snap), nil
}
