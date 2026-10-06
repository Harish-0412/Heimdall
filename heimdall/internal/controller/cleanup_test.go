package controller

import (
	"context"
	"errors"
	"testing"

	v1 "github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/render"
)

func TestCleanupIgnoresRevokedPolicyUnavailableDataAndInvalidConfig(t *testing.T) {
	pe := &v1.PreviewEnvironment{Spec: v1.PreviewEnvironmentSpec{Tenant: "acme", Repository: "acme/shop", PullRequest: 7, EnvironmentID: "preview", Generation: 4, URLSuffix: "abcd", DesiredState: v1.DesiredDestroyed, Config: v1.ConfigSource{Inline: "invalid and no longer approved", SHA256: "wrong"}}}
	b := SpecBuilder{PolicyFor: func(context.Context, string) (config.Policy, error) {
		t.Fatal("cleanup depended on remote policy")
		return config.Policy{}, errors.New("offline")
	}, Data: func(context.Context, string, string) ([]byte, error) {
		t.Fatal("cleanup depended on fixture")
		return nil, nil
	}}
	spec, _, err := b.Build(context.Background(), pe)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := render.CleanupPlan(spec.Context)
	if err != nil || plan.Namespace != "heimdall-pr7-shop-abcd" {
		t.Fatalf("wrong cleanup scope: %v %v", plan, err)
	}
	if _, err = b.Validate(context.Background(), pe); err != nil {
		t.Fatal(err)
	}
	pe.Spec.Tenant = ""
	if _, _, err = b.Build(context.Background(), pe); err == nil {
		t.Fatal("cleanup accepted missing immutable identity")
	}
}
