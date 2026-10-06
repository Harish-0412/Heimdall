package main

import (
	"os"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/controlapi"
)

func TestOperatorSetupDocumentsMatchStrictInput(t *testing.T) {
	for _, path := range []string{"../../config/operator-provision.example.json", "../../config/operator-repository.example.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var value input
		if err := controlapi.StrictJSON(data, &value); err != nil {
			t.Fatalf("documented operator setup rejected: %s: %v", path, err)
		}
	}
}
