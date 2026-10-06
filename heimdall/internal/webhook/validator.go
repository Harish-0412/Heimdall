// Package webhook serves the agent's validating admission webhook for
// PreviewEnvironment objects, and manages its TLS certificate.
//
// The CRD's own schema and CEL rules enforce structure and transitions
// (immutable identity, monotonic generation and reset nonce, "changed inputs
// need a new generation"). The webhook adds what needs Go: the config must
// load under the tenant policy (ADR 0006) and the environment must render.
// It reuses internal/config and internal/render, so admission, the CLI and
// the controller agree exactly.
package webhook

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/controller"
)

// Path is where the webhook is served; the chart's
// ValidatingWebhookConfiguration points at it.
const Path = "/validate-heimdall-dev-v1alpha1-previewenvironment"

// Validator admits PreviewEnvironment objects.
type Validator struct {
	// Specs must not resolve data imports: admission never reads them.
	Specs controller.SpecBuilder
}

var _ admission.Validator[*v1alpha1.PreviewEnvironment] = &Validator{}

// ValidateCreate implements admission.Validator.
func (v *Validator) ValidateCreate(ctx context.Context, pe *v1alpha1.PreviewEnvironment) (admission.Warnings, error) {
	return v.validate(ctx, pe)
}

// ValidateUpdate implements admission.Validator. Metadata-only updates
// (finalizers, labels) and objects being deleted are always admitted, so the
// agent can always release an object.
func (v *Validator) ValidateUpdate(ctx context.Context, old, pe *v1alpha1.PreviewEnvironment) (admission.Warnings, error) {
	if !pe.DeletionTimestamp.IsZero() || equality.Semantic.DeepEqual(old.Spec, pe.Spec) {
		return nil, nil
	}
	return v.validate(ctx, pe)
}

// ValidateDelete implements admission.Validator.
func (v *Validator) ValidateDelete(context.Context, *v1alpha1.PreviewEnvironment) (admission.Warnings, error) {
	return nil, nil
}

func (v *Validator) validate(ctx context.Context, pe *v1alpha1.PreviewEnvironment) (admission.Warnings, error) {
	specs := v.Specs
	specs.Data = nil
	diags, err := specs.Validate(ctx, pe)
	var warnings admission.Warnings
	for _, d := range diags {
		if d.Severity == config.SeverityWarning {
			warnings = append(warnings, fmt.Sprintf("heimdall.yaml line %d: %s: %s", d.Line, d.Code, d.Message))
		}
	}
	if err == nil {
		return warnings, nil
	}
	path := field.NewPath("spec", "config", "inline")
	var f *controller.Failure
	if errors.As(err, &f) && (f.Code == controller.CodeDataUnexpected || f.Code == controller.CodeDataMissing) {
		path = field.NewPath("spec", "data")
	}
	// The value is omitted: a config can be large, and errors carry its
	// positions and codes already.
	return warnings, apierrors.NewInvalid(v1alpha1.GroupVersion.WithKind("PreviewEnvironment").GroupKind(), pe.Name,
		field.ErrorList{field.Invalid(path, "(omitted)", err.Error())})
}
