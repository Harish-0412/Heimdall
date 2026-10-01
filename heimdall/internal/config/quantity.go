package config

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Deliberately tiny parsers for the two quantity formats we accept. Pulling in
// k8s.io/apimachinery just to validate "250m" and "512Mi" would drag a large
// dependency tree into every binary, including the webhook Lambda.

var (
	errBadCPU    = errors.New(`use millicores ("250m") or cores ("1", "0.5")`)
	errBadMemory = errors.New(`use binary units: "256Mi" or "1Gi"`)
)

const maxQuantity = 1 << 20

// parseCPU returns millicores.
func parseCPU(s string) (int, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 || n > maxQuantity {
			return 0, errBadCPU
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1024 {
		return 0, errBadCPU
	}
	milli := int(math.Round(f * 1000))
	if milli < 1 {
		return 0, errBadCPU
	}
	return milli, nil
}

// parseMebibytes returns MiB. It is used for both memory and storage sizes.
func parseMebibytes(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 0
	switch {
	case strings.HasSuffix(s, "Gi"):
		mult, s = 1024, strings.TrimSuffix(s, "Gi")
	case strings.HasSuffix(s, "Mi"):
		mult, s = 1, strings.TrimSuffix(s, "Mi")
	default:
		return 0, errBadMemory
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > maxQuantity {
		return 0, errBadMemory
	}
	return n * mult, nil
}
