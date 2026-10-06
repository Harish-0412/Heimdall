package strictyaml

import (
	"strings"
	"testing"
)

type doc struct {
	GracePeriod string `json:"gracePeriod"`
	Nested      struct {
		Name string `json:"name"`
	} `json:"nested"`
}

func TestUnmarshal(t *testing.T) {
	var d doc
	if err := Unmarshal([]byte("gracePeriod: 1m\nnested: {name: x}\n"), &d); err != nil || d.GracePeriod != "1m" || d.Nested.Name != "x" {
		t.Fatalf("%+v %v", d, err)
	}
	for name, src := range map[string]string{
		"case mismatch":   "graceperiod: 1m\n",
		"unknown field":   "gracePeriod: 1m\nextra: true\n",
		"nested unknown":  "nested: {nam: x}\n",
		"duplicate field": "gracePeriod: 1m\ngracePeriod: 2m\n",
		"malformed":       "gracePeriod: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			var d doc
			if err := Unmarshal([]byte(src), &d); err == nil {
				t.Fatalf("accepted %q", strings.TrimSpace(src))
			}
		})
	}
}
