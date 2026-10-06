package render

import (
	"fmt"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/heimdall-dev/heimdall/internal/config"
)

// ResetJobs reuses the reviewed pod security and credential builders. The
// monotonic nonce distinguishes separate resets without changing deployment
// generation. Retrying one nonce reuses the same immutable Jobs.
func ResetJobs(cfg *config.Config, ctx Context, nonce int64) ([]Object, []Object, error) {
	if nonce < 1 {
		return nil, nil, fmt.Errorf("reset nonce must be positive")
	}
	p, err := Render(cfg, ctx)
	if err != nil {
		return nil, nil, err
	}
	b := &builder{cfg: cfg, ctx: ctx, platform: ctx.Platform.withDefaults(), ns: p.Namespace}
	var database []Object
	if pg := cfg.Dependencies.Postgres; pg != nil {
		job := b.dbAdminJob("reset", pg, nil, `psql -v ON_ERROR_STOP=1 -f `+scriptsDir+`/clone.sql`)
		job.Name = generationName(ctx.Generation, "heimdall-reset", fmt.Sprint(nonce))
		// Reset never unfreezes or re-runs migrations against the baseline.
		database = append(database, job)
	}
	var smoke []Object
	stage, _ := p.Stage(StageSmoke)
	for _, step := range stage.Steps {
		for _, o := range step.Objects {
			j := o.(*batchv1.Job).DeepCopy()
			j.Name = generationName(ctx.Generation, "heimdall-reset-smoke", fmt.Sprint(nonce), j.Spec.Template.Labels["app.kubernetes.io/name"])
			smoke = append(smoke, j)
		}
	}
	return database, smoke, nil
}
