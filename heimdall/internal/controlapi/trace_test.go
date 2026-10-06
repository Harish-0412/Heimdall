package controlapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/controlapi/gen"
)

func TestManualSpecRejectsInvalidTraceBeforePersistence(t *testing.T) {
	for _, value := range []string{"bad-parent", strings.Repeat("a", 129), "00-00000000000000000000000000000000-0123456789abcdef-01"} {
		f := apiFixture()
		s := New(f, Options{})
		spec := apiSpec()
		spec.TraceParent = value
		in := gen.CreateEnvironment{ClusterID: clusterA, RepositoryID: repoA, Name: "env-7"}
		in.Spec, _ = json.Marshal(spec)
		body, _ := json.Marshal(in)
		w := apiRequest(t, s, "POST", "/v1/environments", "admin", string(body), map[string]string{"Idempotency-Key": "create-invalid-trace"})
		if w.Code != 400 || !strings.Contains(w.Body.String(), "config.invalid") {
			t.Fatalf("invalid trace spec reached persistence: %d %s", w.Code, w.Body.String())
		}
	}
}
