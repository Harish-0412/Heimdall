package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxDays = 3650

// Duration is a time.Duration that also understands a "d" (days) suffix, so
// "2d" and "48h" are equivalent. Zero means "unset"; explicit non-positive
// values are rejected at parse time.
type Duration time.Duration

// Std converts to a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the shortest exact form: 2d, 36h, 90m, or Go's default.
func (d Duration) String() string { return FormatDuration(time.Duration(d)) }

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// UnmarshalYAML implements yaml.Unmarshaler. Errors are reported as
// yaml.TypeError so the decoder keeps going and we still get line numbers.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %v", n.Line, err)}}
	}
	*d = Duration(v)
	return nil
}

// ParseDuration parses Go durations ("90m", "1h30m") plus whole days ("2d").
// The result is always positive.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("duration must not be empty (examples: 90m, 48h, 2d)")
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n > maxDays {
			return 0, fmt.Errorf("invalid duration %q (examples: 90m, 48h, 2d)", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q (examples: 90m, 48h, 2d)", s)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}

// FormatDuration renders d in the largest unit that represents it exactly.
func FormatDuration(d time.Duration) string {
	switch {
	case d > 0 && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}
