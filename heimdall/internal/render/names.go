package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// NamespacePrefix starts every preview namespace name. The agent's admission
// policy (charts/heimdall-agent) only lets it manage namespaces with this
// prefix and the heimdall.dev/preview label, so the two must stay in step.
const NamespacePrefix = "heimdall-"

const maxLabel = 63 // DNS label, label value and most object names

var nonDNS = regexp.MustCompile(`[^a-z0-9]+`)

// dnsWord lowercases s and turns every run of characters outside [a-z0-9]
// into one '-', trimming dashes from the ends: "Shop_Flow.v2" -> "shop-flow-v2".
func dnsWord(s string) string {
	return strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// repoName is the sanitized repository name (the part after "owner/").
func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return dnsWord(name)
}

// fitLabel joins head, flex and tail with '-' into a DNS label of at most 63
// characters by shortening flex, which is the only part allowed to lose
// information. head and tail must already fit together; flex is dropped when
// no room is left. It never produces a leading, trailing or double '-'.
func fitLabel(head, flex, tail string) string {
	budget := maxLabel - len(head) - len(tail) - 2
	if budget > 0 && len(flex) > budget {
		flex = flex[:budget]
	}
	flex = strings.Trim(flex, "-")
	if budget <= 0 || flex == "" {
		return head + "-" + tail
	}
	return head + "-" + flex + "-" + tail
}

// NamespaceFor returns the namespace Render gives an environment. The agent's
// sweeper uses it to work out which preview namespaces an authoritative
// desired state accounts for, without rendering every environment.
func NamespaceFor(repo string, pr int, suffix string) string {
	return namespaceName(pr, repo, suffix)
}

// namespaceName is unique per environment (the suffix is) and readable:
// heimdall-pr184-shopflow-x7d2.
func namespaceName(pr int, repo, suffix string) string {
	return fitLabel(fmt.Sprintf("%spr%d", NamespacePrefix, pr), repoName(repo), suffix)
}

// hostLabel is the leftmost DNS label of a preview hostname: one label, so the
// cluster's single wildcard certificate covers every preview (P1 naming rule).
// The primary service gets pr184-shopflow-x7d2, others pr184-shopflow-api-x7d2.
func hostLabel(pr int, repo, service, suffix string) string {
	tail := suffix
	if service != "" {
		tail = service + "-" + suffix
	}
	return fitLabel(fmt.Sprintf("pr%d", pr), repoName(repo), tail)
}

// objectName joins parts with '-'. If the result is longer than a DNS label it
// is truncated and suffixed with a hash of the full name, so distinct inputs
// keep distinct names.
func objectName(parts ...string) string {
	name := strings.Join(parts, "-")
	if len(name) <= maxLabel {
		return name
	}
	return strings.TrimRight(name[:maxLabel-9], "-") + "-" + shortHash(name)
}

// generationName names a per-generation object: heimdall-migrate-g7.
func generationName(gen int64, parts ...string) string {
	return objectName(append(parts, fmt.Sprintf("g%d", gen))...)
}

var labelValueRE = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`)
var nonLabelValue = regexp.MustCompile(`[^-A-Za-z0-9_.]+`)

// labelValue makes s a valid label value. Labels are metadata only (ADR 0005),
// so lossy values are acceptable; the exact value goes in an annotation where
// it matters. A changed value gets a hash suffix so it stays distinctive.
func labelValue(s string) string {
	if len(s) <= maxLabel && labelValueRE.MatchString(s) {
		return s
	}
	clean := strings.Trim(nonLabelValue.ReplaceAllString(s, "-"), "-_.")
	if len(clean) > maxLabel-9 {
		clean = strings.TrimRight(clean[:maxLabel-9], "-_.")
	}
	if clean == "" {
		return shortHash(s)
	}
	return clean + "-" + shortHash(s)
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// envVarSuffix turns a service name into an env var suffix: "my-api" -> "MY_API".
func envVarSuffix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}
