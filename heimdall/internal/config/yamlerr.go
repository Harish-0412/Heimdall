package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	lineRE    = regexp.MustCompile(`^line (\d+): (.*)$`)
	syntaxRE  = regexp.MustCompile(`(?s)^yaml: (?:line (\d+): )?(.*)$`)
	unknownRE = regexp.MustCompile(`^field (\S+) not found in type config\.(\w+)$`)
)

// knownTypes lets us turn yaml.v3's "field x not found in type config.Service"
// into a "did you mean ...?" hint by reflecting over the schema types.
var knownTypes = map[string]reflect.Type{}

func init() {
	for _, v := range []any{
		Config{}, Service{}, Worker{}, Build{}, Health{}, Resources{},
		Requests{}, Dependencies{}, Postgres{}, Redis{}, RabbitMQ{},
		Migrations{}, SmokeTest{}, Preview{},
	} {
		t := reflect.TypeOf(v)
		knownTypes[t.Name()] = t
	}
}

var tagNames = strings.NewReplacer(
	"!!str", "a string",
	"!!int", "a number",
	"!!float", "a number",
	"!!bool", "a boolean",
	"!!map", "a mapping",
	"!!seq", "a list",
	"!!null", "null",
)

// yamlDiagnostics converts errors from yaml.v3 (syntax errors and type errors)
// into Diagnostics with line numbers.
func yamlDiagnostics(err error) Diagnostics {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		out := make(Diagnostics, 0, len(te.Errors))
		for _, msg := range te.Errors {
			out = append(out, typeErrorDiagnostic(msg))
		}
		return out
	}
	d := Diagnostic{Severity: SeverityError, Code: "yaml.syntax", Message: err.Error()}
	if m := syntaxRE.FindStringSubmatch(err.Error()); m != nil {
		d.Line, _ = strconv.Atoi(m[1])
		d.Message = strings.TrimSpace(m[2])
	}
	return Diagnostics{d}
}

func typeErrorDiagnostic(msg string) Diagnostic {
	d := Diagnostic{Severity: SeverityError, Code: "value.invalid", Message: msg}
	if m := lineRE.FindStringSubmatch(msg); m != nil {
		d.Line, _ = strconv.Atoi(m[1])
		d.Message = m[2]
	}
	if m := unknownRE.FindStringSubmatch(d.Message); m != nil {
		field, typeName := m[1], m[2]
		d.Code = "field.unknown"
		d.Message = fmt.Sprintf("unknown field %q in %s", field, strings.ToLower(typeName))
		if t, ok := knownTypes[typeName]; ok {
			fields := yamlFields(t)
			if s := closest(field, fields); s != "" {
				d.Hint = fmt.Sprintf("Did you mean %q?", s)
			} else {
				d.Hint = "Valid fields: " + strings.Join(fields, ", ")
			}
		}
		return d
	}
	if strings.HasPrefix(d.Message, "cannot unmarshal") {
		d.Code = "type.mismatch"
		d.Message = tagNames.Replace(d.Message)
	}
	return d
}

func yamlFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// closest returns the candidate within edit distance 2 of s (case-insensitive),
// or "" if none is close enough.
func closest(s string, candidates []string) string {
	best, bestDist := "", 3
	for _, c := range candidates {
		if d := levenshtein(strings.ToLower(s), strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
