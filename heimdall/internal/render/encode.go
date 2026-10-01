package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
)

// Encode returns obj as YAML: keys sorted, status and other empty server-side
// fields omitted. Output is deterministic, so it can be compared byte for byte.
func Encode(obj Object) ([]byte, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exact; float64 would round above 2^53
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// Status is owned by controllers; a rendered object never sets it. Claim
	// templates embed a whole PVC, status included.
	delete(m, "status")
	if spec, ok := m["spec"].(map[string]any); ok {
		if claims, ok := spec["volumeClaimTemplates"].([]any); ok {
			for _, c := range claims {
				if c, ok := c.(map[string]any); ok {
					delete(c, "status")
				}
			}
		}
	}
	return yaml.Marshal(m)
}

// WriteStep writes a step as a multi-document YAML stream, preceded by a
// comment naming the stage and step. kubectl apply -f accepts it directly.
func WriteStep(w io.Writer, stage StageName, step Step) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# stage: %s, step: %s\n", stage, step.Name)
	for _, obj := range step.Objects {
		doc, err := Encode(obj)
		if err != nil {
			return err
		}
		buf.WriteString("---\n")
		buf.Write(doc)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// WritePlan writes every step of the plan in order, or only those of one
// stage when only is non-empty. Stages without steps get a comment, so the
// output still shows that they were considered.
func WritePlan(w io.Writer, p *Plan, only StageName) error {
	found := false
	for _, s := range p.Stages {
		if only != "" && s.Name != only {
			continue
		}
		found = true
		if len(s.Steps) == 0 {
			if _, err := fmt.Fprintf(w, "# stage: %s (nothing to do for this configuration)\n", s.Name); err != nil {
				return err
			}
			continue
		}
		for _, st := range s.Steps {
			if err := WriteStep(w, s.Name, st); err != nil {
				return err
			}
		}
	}
	if !found {
		return fmt.Errorf("unknown stage %q", only)
	}
	return nil
}

// File is one step rendered to its own file, for applying step by step.
type File struct {
	// Name sorts in apply order: "03-baseline-db-prepare.yaml".
	Name  string
	Stage StageName
	Step  string
	Data  []byte
}

// Files renders each step of the plan into its own numbered file.
func Files(p *Plan) ([]File, error) {
	var out []File
	for _, s := range p.Stages {
		for _, st := range s.Steps {
			var buf bytes.Buffer
			if err := WriteStep(&buf, s.Name, st); err != nil {
				return nil, err
			}
			out = append(out, File{
				Name:  fmt.Sprintf("%02d-%s-%s.yaml", len(out)+1, s.Name, st.Name),
				Stage: s.Name,
				Step:  st.Name,
				Data:  buf.Bytes(),
			})
		}
	}
	return out, nil
}
