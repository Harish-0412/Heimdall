package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/engine"
	"github.com/heimdall-dev/heimdall/internal/render"
)

// DataLoader returns the bytes of an approved import named by a
// PreviewEnvironment (a ConfigMap key in the agent's namespace).
type DataLoader func(ctx context.Context, configMap, key string) ([]byte, error)

// SpecBuilder turns a PreviewEnvironment into the engine's input. The object's
// spec is the whole intent (ADR 0005): nothing is read from labels, status or
// the cluster, except the approved import the spec names explicitly.
type SpecBuilder struct {
	// Policy is the tenant policy configs are loaded with (ADR 0006).
	Policy config.Policy
	// Platform describes this cluster to the renderer.
	Platform render.Platform
	// Data resolves spec.data. Nil means imports cannot be resolved; Build
	// then uses placeholder bytes (admission-time checks only).
	Data DataLoader
}

// Build returns the engine spec and any config warnings. Errors are Failures.
func (b SpecBuilder) Build(ctx context.Context, pe *v1alpha1.PreviewEnvironment) (engine.Spec, config.Diagnostics, error) {
	s := pe.Spec
	sum := sha256.Sum256([]byte(s.Config.Inline))
	if hex.EncodeToString(sum[:]) != s.Config.SHA256 {
		return engine.Spec{}, nil, failf(CodeConfigDigest, false, "spec.config.inline does not match spec.config.sha256")
	}
	cfg, diags := config.Load(strings.NewReader(s.Config.Inline), b.Policy)
	if cfg == nil {
		return engine.Spec{}, diags, failf(CodeConfigInvalid, false, "%s", summarize(diags))
	}
	ctx2 := render.Context{
		Tenant:        s.Tenant,
		Repo:          s.Repository,
		PR:            int(s.PullRequest),
		SHA:           s.Commit,
		Generation:    s.Generation,
		EnvironmentID: s.EnvironmentID,
		Owner:         s.Owner,
		ExpiresAt:     s.ExpiresAt.UTC(),
		URLSuffix:     s.URLSuffix,
		Images:        s.Images,
		Policy:        b.Policy,
		Platform:      b.Platform,
	}
	spec := engine.Spec{Config: cfg, Context: ctx2}

	declared := cfg.Dependencies.Postgres != nil && cfg.Dependencies.Postgres.Seed != ""
	switch {
	case declared && s.Data == nil:
		return engine.Spec{}, diags, failf(CodeDataMissing, false,
			"the config declares an import (%s) but spec.data names no approved data", cfg.Dependencies.Postgres.Seed)
	case !declared && s.Data != nil:
		return engine.Spec{}, diags, failf(CodeDataUnexpected, false, "spec.data is set but the config declares no import")
	case s.Data != nil:
		a := s.Data.Approval
		spec.SeedApproval = &engine.SeedApproval{SHA256: a.SHA256, ApprovedBy: a.ApprovedBy, Reason: a.Reason, Sanitised: a.Sanitised}
		if b.Data == nil {
			// Admission time: the bytes are checked against the approval when
			// the agent deploys, so a placeholder suffices for rendering.
			spec.Context.Seed = []byte("-- resolved at deployment\n")
			spec.SeedApproval = nil
			break
		}
		data, err := b.Data(ctx, s.Data.ConfigMap, s.Data.Key)
		if err != nil {
			return engine.Spec{}, diags, &Failure{Code: CodeDataUnavailable, Retryable: true, cause: err,
				Message: fmt.Sprintf("cannot read approved data %s/%s", s.Data.ConfigMap, s.Data.Key)}
		}
		spec.Context.Seed = data
	}
	return spec, diags, nil
}

// Validate is Build plus a full render, for admission: it catches unpinned
// images, bad platform combinations and other render errors before the
// object is accepted.
func (b SpecBuilder) Validate(ctx context.Context, pe *v1alpha1.PreviewEnvironment) (config.Diagnostics, error) {
	spec, diags, err := b.Build(ctx, pe)
	if err != nil {
		return diags, err
	}
	if _, err := render.Render(spec.Config, spec.Context); err != nil {
		return diags, failf("engine.spec_invalid", false, "%v", err)
	}
	return diags, nil
}

// summarize lists the first few error diagnostics in one line.
func summarize(diags config.Diagnostics) string {
	var parts []string
	for _, d := range diags {
		if d.Severity != config.SeverityError {
			continue
		}
		loc := d.Path
		if d.Line > 0 {
			loc = fmt.Sprintf("line %d (%s)", d.Line, d.Path)
		}
		parts = append(parts, fmt.Sprintf("%s: %s: %s", loc, d.Code, d.Message))
		if len(parts) == 3 {
			break
		}
	}
	if n := diags.Errors(); n > len(parts) {
		parts = append(parts, fmt.Sprintf("and %d more", n-len(parts)))
	}
	return "heimdall.yaml is invalid: " + strings.Join(parts, "; ")
}
