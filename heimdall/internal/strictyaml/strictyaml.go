// Package strictyaml decodes YAML into JSON-tagged Go types strictly:
// unknown fields, duplicate fields and fields whose case differs from the
// tag are all errors.
//
// sigs.k8s.io/yaml's UnmarshalStrict is not enough on its own: it finishes
// with encoding/json, which matches field names case-insensitively, so a typo
// such as "graceperiod" would silently set "gracePeriod". Kubernetes' own
// strict decoder (sigs.k8s.io/json) is case-sensitive.
package strictyaml

import (
	"errors"
	"fmt"

	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// Unmarshal decodes data into v.
func Unmarshal(data []byte, v any) error {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return err
	}
	strict, err := kjson.UnmarshalStrict(j, v)
	if err != nil {
		return err
	}
	if len(strict) > 0 {
		return fmt.Errorf("strict decoding: %w", errors.Join(strict...))
	}
	return nil
}
