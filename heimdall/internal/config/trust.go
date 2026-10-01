package config

import "fmt"

// CompareToBaseline enforces the preview configuration trust model (ADR 0006).
//
// baseline is the heimdall.yaml from the protected default branch; pr is the
// file from the pull request. Both must already have passed Load. A PR may
// freely change application-level settings (builds, ports, commands, tests,
// non-secret env), but it may not *loosen* anything that carries risk or cost:
// anything beyond the baseline must be reviewed and merged into the default
// branch first.
//
// Diagnostics are positioned using pr's source map. The result is sorted.
func CompareToBaseline(baseline, pr *Config) Diagnostics {
	c := &comparer{base: baseline, pr: pr}
	c.visibility()
	c.ttl()
	c.postgresStorage()
	c.workloads()
	c.diags.Sort()
	return c.diags
}

type comparer struct {
	base, pr *Config
	diags    Diagnostics
}

func (c *comparer) deny(code, path, hint, format string, args ...any) {
	pos := c.pr.src.Lookup(path)
	c.diags = append(c.diags, Diagnostic{
		Severity: SeverityError, Code: code, Path: path,
		Line: pos.Line, Column: pos.Column,
		Message: fmt.Sprintf(format, args...), Hint: hint,
	})
}

const mergeFirst = "Make this change in a pull request to the default branch first; previews then accept it."

func (c *comparer) visibility() {
	if visibilityRank(c.pr.Preview.Visibility) > visibilityRank(c.base.Preview.Visibility) {
		c.deny("trust.visibility", "preview.visibility", mergeFirst,
			"visibility %q is more open than the default branch's %q",
			c.pr.Preview.Visibility, c.base.Preview.Visibility)
	}
}

func (c *comparer) ttl() {
	if c.pr.Preview.TTL > c.base.Preview.TTL {
		c.deny("trust.ttl", "preview.ttl", mergeFirst,
			"ttl %s is longer than the default branch's %s", c.pr.Preview.TTL, c.base.Preview.TTL)
	}
}

func (c *comparer) postgresStorage() {
	p := c.pr.Dependencies.Postgres
	if p == nil {
		return
	}
	ceiling, _ := parseMebibytes(defaultPostgresStorage)
	if b := c.base.Dependencies.Postgres; b != nil {
		ceiling, _ = parseMebibytes(b.Storage)
	}
	if got, _ := parseMebibytes(p.Storage); got > ceiling {
		c.deny("trust.resources", "dependencies.postgres.storage", mergeFirst,
			"database storage %s exceeds the default branch's %dMi", p.Storage, ceiling)
	}
}

// workloads checks secrets and resources for every service and worker.
func (c *comparer) workloads() {
	allowedSecrets := map[string]bool{}
	for _, w := range c.base.workloadList() {
		for _, s := range w.secrets {
			allowedSecrets[s] = true
		}
	}
	baseByName := map[string]workloadInfo{}
	for _, w := range c.base.workloadList() {
		baseByName[w.kind+"."+w.name] = w
	}

	for _, w := range c.pr.workloadList() {
		base := w.kind + "." + w.name

		for i, s := range w.secrets {
			if !allowedSecrets[s] {
				c.deny("trust.secret", fmt.Sprintf("%s.secrets[%d]", base, i),
					"A pull request cannot gain access to new secrets. "+mergeFirst,
					"secret %q is not used by any workload on the default branch", s)
			}
		}

		// New workloads are capped at the defaults; existing ones at their
		// default-branch size.
		ceilCPU, ceilMem := w.defaultCPU, w.defaultMem
		if b, ok := baseByName[base]; ok {
			ceilCPU, ceilMem = b.res.CPU, b.res.Memory
		}
		if cpu, max := milli(w.res.CPU), milli(ceilCPU); cpu > max {
			c.deny("trust.resources", base+".resources.cpu", mergeFirst,
				"cpu %s exceeds the default branch's %s for %s", w.res.CPU, ceilCPU, base)
		}
		if mem, max := mebi(w.res.Memory), mebi(ceilMem); mem > max {
			c.deny("trust.resources", base+".resources.memory", mergeFirst,
				"memory %s exceeds the default branch's %s for %s", w.res.Memory, ceilMem, base)
		}
	}
}

type workloadInfo struct {
	kind, name             string
	secrets                []string
	res                    Resources
	defaultCPU, defaultMem string
}

func (c *Config) workloadList() []workloadInfo {
	out := make([]workloadInfo, 0, len(c.Services)+len(c.Workers))
	for _, n := range sortedKeys(c.Services) {
		s := c.Services[n]
		out = append(out, workloadInfo{"services", n, s.Secrets, s.Resources, defaultServiceCPU, defaultServiceMemory})
	}
	for _, n := range sortedKeys(c.Workers) {
		w := c.Workers[n]
		out = append(out, workloadInfo{"workers", n, w.Secrets, w.Resources, defaultWorkerCPU, defaultWorkerMemory})
	}
	return out
}

// milli and mebi parse already-validated quantities; invalid input maps to 0.
func milli(s string) int { v, _ := parseCPU(s); return v }
func mebi(s string) int  { v, _ := parseMebibytes(s); return v }
