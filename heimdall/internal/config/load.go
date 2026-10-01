package config

import (
	"bytes"
	"errors"
	"io"

	"gopkg.in/yaml.v3"
)

// MaxFileSize bounds how much of a config file is read. The API parses files
// from untrusted pull requests, so input size must be capped up front.
const MaxFileSize = 256 << 10

// Load reads, strictly parses, validates and normalises a heimdall.yaml.
//
// The returned Config is non-nil only if there are no error diagnostics; it
// then has every default applied. Warnings may accompany a valid Config.
func Load(r io.Reader, policy Policy) (*Config, Diagnostics) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return nil, single("file.unreadable", err.Error())
	}
	if len(data) > MaxFileSize {
		return nil, single("file.too_large", "config file exceeds the 256 KiB limit")
	}

	// Pass 1: decode to a node tree. This gives us positions for every key and
	// catches syntax errors and multi-document files before typed decoding.
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, single("file.empty", "config file is empty")
		}
		return nil, yamlDiagnostics(err)
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, single("file.multiple_documents", "config must contain a single YAML document")
	case !errors.Is(err, io.EOF):
		return nil, yamlDiagnostics(err)
	}

	// Pass 2: strict typed decode. Unknown fields are errors, not silently
	// ignored, because a typo like "dependson" would otherwise change behaviour.
	var cfg Config
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	var decodeDiags Diagnostics
	if err := strict.Decode(&cfg); err != nil {
		decodeDiags = yamlDiagnostics(err)
		// Unknown fields leave the rest of the struct fully populated, so keep
		// validating and report everything in one pass. Any other decode error
		// (wrong type, bad duration) can cascade into misleading follow-ups.
		for _, d := range decodeDiags {
			if d.Code != "field.unknown" {
				decodeDiags.Sort()
				return nil, decodeDiags
			}
		}
	}

	v := &validator{cfg: &cfg, sm: newSourceMap(&doc), policy: policy, diags: decodeDiags}
	v.nulls(&doc)
	v.run()
	if !v.diags.HasErrors() {
		// Checks that compare a value with a default (a request with its
		// defaulted limit, totals including default sizes) run afterwards.
		cfg.applyDefaults(policy)
		v.requests()
		v.totals()
	}
	v.diags.Sort()
	if v.diags.HasErrors() {
		return nil, v.diags
	}
	cfg.src = v.sm
	return &cfg, v.diags
}

func single(code, msg string) Diagnostics {
	return Diagnostics{{Severity: SeverityError, Code: code, Message: msg}}
}
