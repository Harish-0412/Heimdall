package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
)

func TestEmbeddedOpenAPIAndGeneratedClientMatchServer(t *testing.T) {
	ctx := context.Background()
	spec, err := gen.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	if err = spec.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	f := apiFixture()
	srv := httptest.NewServer(New(f, Options{}).Handler())
	defer srv.Close()
	client, err := gen.NewClientWithResponses(srv.URL, gen.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer admin")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	env, err := client.GetEnvironmentWithResponse(ctx, "env-7")
	if err != nil || env.JSON200 == nil {
		t.Fatalf("generated client: %+v %v", env, err)
	}
	var document any
	if err = json.Unmarshal(env.Body, &document); err != nil {
		t.Fatal(err)
	}
	if err = spec.Components.Schemas["Environment"].Value.VisitJSON(document); err != nil {
		t.Fatalf("environment response violates OpenAPI: %v", err)
	}
	policy, err := client.GetPolicyWithResponse(ctx)
	if err != nil || policy.JSON200 == nil {
		t.Fatalf("generated client policy: %+v %v", policy, err)
	}
	if err = json.Unmarshal(policy.Body, &document); err != nil {
		t.Fatal(err)
	}
	if err = spec.Components.Schemas["Policy"].Value.VisitJSON(document); err != nil {
		t.Fatalf("policy response violates OpenAPI: %v", err)
	}
	for _, route := range []string{"/v1/agent/register", "/v1/agent/refresh"} {
		found := false
		for _, param := range spec.Paths.Value(route).Post.Parameters {
			if param.Value.Name == "Idempotency-Key" && param.Value.Required {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s lacks persistent rotation nonce", route)
		}
	}
	if spec.Components.Schemas["DesiredSnapshot"].Value.Properties["tenantSlug"] == nil {
		t.Fatal("empty snapshots cannot warm tenant admission policy")
	}
}
