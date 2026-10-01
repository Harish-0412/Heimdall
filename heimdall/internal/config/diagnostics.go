package config

import "sort"

// Severity of a Diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic is a single finding about a config file. Codes are stable,
// dotted identifiers (for example "service.port.missing") so that tooling, docs
// and tests can refer to them without matching on prose.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Code     string   `json:"code"`
	// Path is the logical location, for example "services.api.port".
	Path string `json:"path,omitempty"`
	// Line and Column are 1-based; 0 means unknown.
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// Diagnostics is an ordered list of findings.
type Diagnostics []Diagnostic

// HasErrors reports whether any diagnostic is an error.
func (d Diagnostics) HasErrors() bool { return d.count(SeverityError) > 0 }

// Errors returns the number of error diagnostics.
func (d Diagnostics) Errors() int { return d.count(SeverityError) }

// Warnings returns the number of warning diagnostics.
func (d Diagnostics) Warnings() int { return d.count(SeverityWarning) }

// Codes returns the diagnostic codes in order. Handy in tests.
func (d Diagnostics) Codes() []string {
	out := make([]string, len(d))
	for i, x := range d {
		out[i] = x.Code
	}
	return out
}

func (d Diagnostics) count(s Severity) int {
	n := 0
	for _, x := range d {
		if x.Severity == s {
			n++
		}
	}
	return n
}

// Sort orders findings by file position so output reads top to bottom.
func (d Diagnostics) Sort() {
	sort.SliceStable(d, func(i, j int) bool {
		if d[i].Line != d[j].Line {
			return d[i].Line < d[j].Line
		}
		if d[i].Column != d[j].Column {
			return d[i].Column < d[j].Column
		}
		return d[i].Path < d[j].Path
	})
}
